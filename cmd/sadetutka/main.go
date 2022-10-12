package main

import (
	"bytes"
	"context"
	_ "image/png"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/function61/chrome-server/pkg/chromeserverclient"
	"github.com/function61/gokit/aws/lambdautils"
	"github.com/function61/gokit/aws/s3facade"
	"github.com/function61/gokit/dynversion"
	"github.com/function61/gokit/ezhttp"
	"github.com/function61/gokit/jsonfile"
	"github.com/function61/gokit/logex"
	"github.com/function61/gokit/osutil"
	"github.com/joonas-fi/sadetutka"
	"github.com/spf13/cobra"
)

// defined in script.js
type scriptDataOutput struct {
	FrameUrls    []string `json:"frameUrls"`
	MeteogramUrl string   `json:"meteogramUrl"`
}

func main() {
	if lambdautils.InLambda() {
		// we just assume it's a CloudWatch scheduler trigger so drop input payload
		lambda.StartHandler(lambdautils.NoPayloadAdapter(func(ctx context.Context) error { return logic(ctx, false) }))
		return
	}

	debug := false

	cmd := &cobra.Command{
		Use:     os.Args[0],
		Short:   "Sadetutka",
		Version: dynversion.Version,
		Args:    cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			osutil.ExitIfError(logic(
				osutil.CancelOnInterruptOrTerminate(logex.StandardLogger()),
				debug))
		},
	}

	cmd.Flags().BoolVarP(&debug, "debug", "", debug, "Prints scraper raw output")

	osutil.ExitIfError(cmd.Execute())
}

func logic(ctx context.Context, debug bool) error {
	bucket, err := s3facade.Bucket("files.function61.com", nil, "us-east-1")
	if err != nil {
		return err
	}

	workdir, err := ioutil.TempDir("", "sadetutka-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workdir)

	// for convenience, embed the script so we can have single-binary Lambda, but for local editing if
	// it exists in the workdir, use that (so you don't have to compile to iterate on the script)
	script, err := fs.ReadFile(newOverlayFs(sadetutka.ScraperScript, os.DirFS(".")), "scraperscript.js")
	if err != nil {
		return err
	}

	chromeServer, err := chromeserverclient.New(
		chromeserverclient.Function61,
		chromeserverclient.TokenFromEnv)
	if err != nil {
		return err
	}

	log.Println("executing scraper")

	scriptOuput := &scriptDataOutput{}
	output, err := chromeServer.Run(ctx, string(script), scriptOuput, &chromeserverclient.Options{
		ErrorAutoScreenshot: true,
	})
	if err != nil {
		return err
	}

	if debug {
		return jsonfile.Marshal(os.Stdout, output)
	}

	localFrameFilenames, err := downloadFilesConcurrently(
		ctx,
		scriptOuput.FrameUrls,
		workdir)
	if err != nil {
		return err
	}

	gifName := filepath.Join(workdir, "latest.gif")

	log.Printf("making %s", gifName)

	if err := createGifFromFrames(gifName, localFrameFilenames); err != nil {
		return err
	}

	log.Println("uploading GIF")

	if err := uploadFile(
		ctx,
		gifName,
		"sadetutka/latest.gif",
		"image/gif",
		bucket,
	); err != nil {
		return err
	}

	log.Println("downloading meteogram")

	meteogram := &bytes.Buffer{}

	res, err := ezhttp.Get(ctx, scriptOuput.MeteogramUrl)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if _, err := io.Copy(meteogram, res.Body); err != nil {
		return err
	}

	log.Println("uploading meteogram")

	return upload(
		ctx,
		bytes.NewReader(meteogram.Bytes()),
		"sadetutka/meteogram.png",
		"image/png",
		bucket)
}
