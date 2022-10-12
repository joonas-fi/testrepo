package main

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"image/gif"
	_ "image/png"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"

	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/function61/gokit/aws/s3facade"
	"github.com/function61/gokit/ezhttp"
	"github.com/function61/gokit/syncutil"
)

func uploadFile(
	ctx context.Context,
	from string,
	key string,
	contentType string,
	bucket *s3facade.BucketContext,
) error {
	file, err := os.Open(from)
	if err != nil {
		return err
	}
	defer file.Close()

	return upload(ctx, file, key, contentType, bucket)
}

func upload(
	ctx context.Context,
	content io.ReadSeeker,
	key string,
	contentType string,
	bucket *s3facade.BucketContext,
) error {
	_, err := bucket.S3.PutObjectWithContext(ctx, &s3.PutObjectInput{
		Bucket:      bucket.Name,
		Key:         &key,
		Body:        content,
		ContentType: &contentType,
	})
	return err
}

func createGifFromFrames(gifName string, localFrameFilenames []string) error {
	images := []*image.Paletted{}

	pal := color.Palette(palette.Plan9)
	var firstBounds *image.Rectangle

	decodeOneImage := func(localFrameFilename string) error {
		f, err := os.Open(localFrameFilename)
		if err != nil {
			return err
		}
		defer f.Close()

		// this is (most probably) non-paletted PNG, so we have to transform it into a
		// paletted image
		origImg, _, err := image.Decode(f)
		if err != nil {
			return err
		}

		bounds := origImg.Bounds()
		if firstBounds == nil {
			firstBounds = &bounds
		}
		if !firstBounds.Eq(bounds) {
			return errors.New("mismatching frame sizes for GIF")
		}

		paletted := image.NewPaletted(bounds, pal)

		draw.FloydSteinberg.Draw(paletted, bounds, origImg, bounds.Min)

		images = append(images, paletted)

		return nil
	}

	for _, localFrameFilename := range localFrameFilenames {
		if err := decodeOneImage(localFrameFilename); err != nil {
			return err
		}
	}

	gifFile, err := os.Create(gifName)
	if err != nil {
		return err
	}
	defer gifFile.Close()

	return gif.EncodeAll(gifFile, &gif.GIF{
		Image: images,
		Delay: sameDelayForAllImages(75, images),
		Config: image.Config{
			ColorModel: pal,
			Width:      firstBounds.Max.X,
			Height:     firstBounds.Max.Y,
		},
	})
}

func sameDelayForAllImages(delay int, images []*image.Paletted) []int {
	delays := make([]int, len(images))
	for i := 0; i < len(delays); i++ {
		delays[i] = delay
	}
	return delays
}

func downloadFile(ctx context.Context, url string, outname string) error {
	log.Printf("downloading %s", url)

	res, err := ezhttp.Get(ctx, url)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	file, err := os.Create(outname)
	if err != nil {
		return err
	}

	_, err = io.Copy(file, res.Body)
	return err
}

func downloadFilesConcurrently(
	ctx context.Context,
	urls []string,
	workdir string,
) ([]string, error) {
	type fileToDownload struct {
		url      string
		filename string
	}

	work := make(chan fileToDownload)
	// after downloading, these are in order that we can feed to GIF maker
	localFilenames := []string{}

	// before adding concurrency (n=3) took 23 sec, after 8 sec. (one sample)
	return localFilenames, syncutil.Concurrently(ctx, 3, func(ctx context.Context) error {
		for frame := range work {
			if err := downloadFile(ctx, frame.url, frame.filename); err != nil {
				return err
			}
		}

		return nil
	}, func(workersCancel context.Context) {
		defer close(work)

		for _, frameUrl := range urls {
			frame := fileToDownload{frameUrl, filepath.Join(workdir, path.Base(frameUrl))}
			localFilenames = append(localFilenames, frame.filename)
			select {
			case work <- frame:
				continue
			case <-workersCancel.Done():
				// worker(s) errored -> stop submitting work. will carry on to the break
				// (break here would break only from select)
			}
			break
		}
	})
}

type overlayFs struct {
	lower fs.FS
	upper fs.FS
}

// makes an "overlay FS", where files are accessed from "upper" (usually R/W) dir first, and if not exists,
// then from "lower" (usually read-only). for semantics see https://wiki.archlinux.org/index.php/Overlay_filesystem
func newOverlayFs(lower fs.FS, upper fs.FS) fs.FS {
	return &overlayFs{lower, upper}
}

func (o *overlayFs) Open(name string) (fs.File, error) {
	upperFile, err := o.upper.Open(name)
	if err != nil {
		if os.IsNotExist(err) {
			return o.lower.Open(name)
		} else {
			return nil, err
		}
	}

	return upperFile, nil
}
