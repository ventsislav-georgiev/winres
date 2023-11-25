package winres

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"sort"

	"github.com/nfnt/resize"
)

// Icon describes a Windows icon.
//
// This structure must only be created by constructors:
// NewIconFromImages, NewIconFromResizedImage, LoadICO
type Icon struct {
	Images []IconImage
}

var DefaultIconSizes = []int{256, 64, 48, 32, 16}

// NewIconFromImages makes an icon from a list of images.
//
// This converts every image to 32bpp PNG.
func NewIconFromImages(images []image.Image) (*Icon, error) {
	icon := Icon{}

	for _, img := range images {
		if err := icon.addImage(img); err != nil {
			return nil, err
		}
	}

	return &icon, nil
}

// NewIconFromResizedImage makes an icon from a single Image by resizing it.
//
// If sizes is nil, the icon will be resized to: 256px, 64px, 48px, 32px, 16px.
func NewIconFromResizedImage(img image.Image, sizes []int) (*Icon, error) {
	if sizes == nil {
		sizes = DefaultIconSizes
	}
	if len(sizes) > 30 {
		return nil, errors.New(errTooManyIconSizes)
	}

	icon := Icon{}
	for _, s := range sizes {
		if err := icon.addImage(resizeImage(img, s)); err != nil {
			return nil, err
		}
	}

	return &icon, nil
}

// LoadICO loads an ICO file and returns an icon, ready to embed in a resource set.
func LoadICO(ico io.ReadSeeker) (*Icon, error) {
	hdr := iconDirHeader{}
	if err := binaryRead(ico, &hdr); err != nil {
		return nil, err
	}

	if hdr.Type != 1 || hdr.Reserved != 0 {
		return nil, errors.New(errNotICO)
	}

	entries := make([]iconFileDirEntry, hdr.Count)
	if err := binaryRead(ico, entries); err != nil {
		return nil, err
	}

	icon := &Icon{}
	for _, e := range entries {
		// Arbitrary limit: no more than 10MB per image, so we can blindly allocate bytes and try to read them.
		if e.BytesInRes > 0xA00000 {
			return nil, fmt.Errorf(errImageLengthTooBig)
		}
		if _, err := ico.Seek(int64(e.ImageOffset), io.SeekStart); err != nil {
			return nil, err
		}
		img := make([]byte, e.BytesInRes)
		if err := readFull(ico, img); err != nil {
			return nil, err
		}
		icon.Images = append(icon.Images, IconImage{
			Info:  e.IconInfo,
			Image: img,
		})
	}

	return icon, nil
}

// SaveICO saves an icon as an ICO file.
func (icon *Icon) SaveICO(ico io.Writer) error {
	err := binary.Write(ico, binary.LittleEndian, &iconDirHeader{
		Type:  1,
		Count: uint16(len(icon.Images)),
	})
	if err != nil {
		return err
	}

	var (
		pos    = sizeOfIconDirHeader
		hdrLen = sizeOfIconDirHeader + len(icon.Images)*sizeOfIconFileDirEntry
		offset = hdrLen
	)

	icon.order()
	for i := range icon.Images {
		err = binary.Write(ico, binary.LittleEndian, &iconFileDirEntry{
			IconInfo:    icon.Images[i].Info,
			ImageOffset: uint32(offset),
		})
		if err != nil {
			return err
		}
		offset += len(icon.Images[i].Image)
		pos += sizeOfIconFileDirEntry
	}

	for i := range icon.Images {
		_, err = ico.Write(icon.Images[i].Image)
		if err != nil {
			return err
		}
	}

	return nil
}

// SetIcon adds the icon to the resource set.
//
// The first icon will be the application's icon, as shown in Windows Explorer.
// That means:
//  1. First name in case-sensitive ascending order, or else...
//  2. First ID in ascending order
//
func (rs *ResourceSet) SetIcon(resID Identifier, icon *Icon) error {
	return rs.SetIconTranslation(resID, LCIDNeutral, icon)
}

// SetIconTranslation adds the icon to a specific language in the resource set.
//
// The first icon will be the application's icon, as shown in Windows Explorer.
// That means:
//  1. First name in case-sensitive ascending order, or else...
//  2. First ID in ascending order
//
func (rs *ResourceSet) SetIconTranslation(resID Identifier, langID uint16, icon *Icon) error {
	b := &bytes.Buffer{}
	binary.Write(b, binary.LittleEndian, iconDirHeader{
		Type:  1,
		Count: uint16(len(icon.Images)),
	})

	icon.order()

	for _, img := range icon.Images {
		id := rs.lastIconID + 1

		binary.Write(b, binary.LittleEndian, iconResDirEntry{
			IconInfo: img.Info,
			Id:       id,
		})

		if err := rs.Set(RT_ICON, ID(id), LCIDNeutral, img.Image); err != nil {
			return err
		}
	}
	return rs.Set(RT_GROUP_ICON, resID, langID, b.Bytes())
}

// GetIcon extracts an icon from a resource set.
func (rs *ResourceSet) GetIcon(resID Identifier) (*Icon, error) {
	return rs.GetIconTranslation(resID, rs.firstLang(RT_GROUP_ICON, resID))
}

// GetIconTranslation extracts an icon from a specific language of the resource set.
func (rs *ResourceSet) GetIconTranslation(resID Identifier, langID uint16) (*Icon, error) {
	data := rs.Get(RT_GROUP_ICON, resID, langID)
	if data == nil {
		return nil, errors.New(errGroupNotFound)
	}

	in := bytes.NewReader(data)
	hdr := iconDirHeader{}
	err := binaryRead(in, &hdr)
	if err != nil || hdr.Type != 1 || hdr.Reserved != 0 {
		return nil, errors.New(errInvalidGroup)
	}

	icon := &Icon{}
	for i := 0; i < int(hdr.Count); i++ {
		entry := iconResDirEntry{}
		err := binaryRead(in, &entry)
		if err != nil {
			return nil, errors.New(errInvalidGroup)
		}
		img := rs.Get(RT_ICON, ID(entry.Id), rs.firstLang(RT_ICON, ID(entry.Id)))
		if img == nil {
			return nil, errors.New(errIconMissing)
		}
		icon.Images = append(icon.Images, IconImage{
			Info:  entry.IconInfo,
			Image: img,
		})
	}

	return icon, nil
}

// An icon is made of an icon directory and actual icons.
//
// The icon directory is made of a header and entries.
//
// Directory entries are slightly different between ICO files and RT_GROUP_ICON resources.
//
// https://devblogs.microsoft.com/oldnewthing/20120720-00/?p=7083
// https://docs.microsoft.com/en-us/previous-versions/ms997538

// iconDirHeader is the binary format of an icon directory header.
type iconDirHeader struct {
	Reserved uint16
	Type     uint16
	Count    uint16
}

const sizeOfIconDirHeader = 6

// iconFileDirEntry is the binary format of an icon directory entry, in an ICO file.
type iconFileDirEntry struct {
	IconInfo
	ImageOffset uint32
}

const sizeOfIconFileDirEntry = 16

// iconResDirEntry is the binary format of an icon directory entry, in an RT_GROUP_ICON resource.
type iconResDirEntry struct {
	IconInfo
	Id uint16
}

// IconInfo is the common part of iconResDirEntry and iconFileDirEntry.
type IconInfo struct {
	Width      uint8
	Height     uint8
	ColorCount uint8
	Reserved   uint8
	Planes     uint16
	BitCount   uint16
	BytesInRes uint32
}

type IconImage struct {
	Info  IconInfo
	Image []byte
}

// This makes a testing error reporting possible
var pngEncode = png.Encode

func (icon *Icon) addImage(img image.Image) error {
	bounds := img.Bounds()
	if bounds.Empty() {
		return errors.New(errInvalidImageDimensions)
	}
	if bounds.Size().X > 256 || bounds.Size().Y > 256 {
		return errors.New(errImageTooBig)
	}

	img = imageInSquareNRGBA(img, true)
	bounds = img.Bounds()
	buf := &bytes.Buffer{}
	if err := pngEncode(buf, img); err != nil {
		return err
	}

	icon.Images = append(icon.Images, IconImage{
		Info: IconInfo{
			Width:      uint8(bounds.Size().X), // 0 means 256
			Height:     uint8(bounds.Size().Y), // 0 means 256
			ColorCount: 0,                      // should be defined as 1 << BitCount only if BitCount < 8
			Reserved:   0,
			Planes:     1,
			BitCount:   32,
			BytesInRes: uint32(buf.Len()),
		},
		Image: buf.Bytes(),
	})

	return nil
}

func (icon *Icon) order() {
	// Sort images by descending size and quality
	sort.SliceStable(icon.Images, func(i, j int) bool {
		img1, img2 := &icon.Images[i].Info, &icon.Images[j].Info
		return img1.BitCount > img2.BitCount ||
			img1.BitCount == img2.BitCount && int(img1.Width-1)+1 > int(img2.Width-1)+1
	})
}

func resizeImage(img image.Image, size int) image.Image {
	var (
		sz   = img.Bounds().Size()
		w, h = size, size
	)

	if sz.X < sz.Y {
		w = 0
	} else if sz.X > sz.Y {
		h = 0
	}

	return resize.Resize(uint(w), uint(h), img, resize.Lanczos2)
}

func imageInSquareNRGBA(img image.Image, center bool) image.Image {
	w, h := img.Bounds().Size().X, img.Bounds().Size().Y
	if w == h && img.ColorModel() == color.NRGBAModel {
		return img
	}

	length := w
	if length < h {
		length = h
	}

	offset := image.Point{
		X: -img.Bounds().Min.X,
		Y: -img.Bounds().Min.Y,
	}
	if center {
		offset.X -= (w - length) / 2
		offset.Y -= (h - length) / 2
	}

	square := image.NewNRGBA(image.Rectangle{Max: image.Point{X: length, Y: length}})
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			square.Set(x+offset.X, y+offset.Y, img.At(x, y))
		}
	}

	return square
}
