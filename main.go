package main

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/klauspost/pgzip"
	"github.com/openSUSE/umoci"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tych0/octoci/pool"
	"github.com/urfave/cli"
)

var (
	version = ""
)

func main() {
	app := cli.NewApp()
	app.Name = "octoci"
	app.Usage = "octoci octopus merges rootfses into an OCI image"
	app.Version = version
	app.Commands = []cli.Command{buildCmd}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %+v", err)
	}
}

var buildCmd = cli.Command{
	Name:   "build",
	Usage:  "builds an octoci image",
	Action: doBuild,
	Flags:  []cli.Flag{
		cli.StringFlag{
			Name:  "oci-dir",
			Usage: "the output OCI dir to use",
			Value: "oci",
		},
		cli.StringFlag{
			Name:  "tag",
			Usage: "the output tag to write",
			Value: "octoci",
		},
	},
	ArgsUsage: `[base-image] [rootfses]

[base-image] is a skopeo compatible URL for the base image.

[rootfses] is a \n separated list of directories to octomerge.`,
}

var otherFailure = fmt.Errorf("got other failure")

func doBuild(ctx *cli.Context) error {
	if len(ctx.Args()) != 2 {
		return fmt.Errorf("wrong number of arguments")
	}

	baseImage := ctx.Args()[0]
	rootfsesFile := ctx.Args()[1]

	rootfsesFileRaw, err := ioutil.ReadFile(rootfsesFile)
	if err != nil {
		return err
	}

	rootfses := strings.Split(strings.TrimSpace(string(rootfsesFileRaw)), "\n")

	output, err := exec.Command(
		"skopeo",
		"--insecure-policy",
		"copy",
		"--src-tls-verify=false",
		baseImage,
		fmt.Sprintf("oci:%s:%s", ctx.String("oci-dir"), ctx.String("tag")),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("image import failed: %s: %s", err, string(output))
	}

	oci, err := umoci.OpenLayout(ctx.String("oci-dir"))
	if err != nil {
		return err
	}
	// Let's GC: if there were any errors and we put some blobs, we don't
	// want to leave those around. Or, if this was a repeat build and
	// generated new blobs, we don't want to leave the old ones around
	// either.
	defer oci.GC()
	defer oci.Close()

	tasks := make([]rootfsProcessor, len(rootfses))
	tp := pool.New(runtime.NumCPU())

	for i, rootfs := range rootfses {
		tasks[i].oci = oci
		tasks[i].rootfs = rootfs

		tp.Add((&tasks[i]).addBlob)
	}

	fmt.Println("done adding jobs")
	tp.DoneAddingJobs()

	err = tp.Run()
	if err != nil {
		return err
	}

	manifest, err := oci.LookupManifest(ctx.String("tag"))
	if err != nil {
		return err
	}

	config, err := oci.LookupConfig(manifest.Config)
	if err != nil {
		return err
	}

	for _, task := range tasks {
		config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, task.diffID)
		manifest.Layers = append(manifest.Layers, task.layerDesc)
	}

	digest, size, err := oci.PutBlobJSON(config)
	if err != nil {
		return err
	}

	manifest.Config = ispec.Descriptor{
		MediaType: ispec.MediaTypeImageConfig,
		Digest:    digest,
		Size:      size,
	}

	digest, size, err = oci.PutBlobJSON(manifest)
	if err != nil {
		return err
	}

	err = oci.UpdateReference(ctx.String("tag"), ispec.Descriptor{
		MediaType: ispec.MediaTypeImageManifest,
		Digest:    digest,
		Size:      size,
	})
	if err != nil {
		return err
	}

	return nil
}

type rootfsProcessor struct {
	oci       *umoci.Layout
	rootfs    string
	diffID    digest.Digest
	layerDesc ispec.Descriptor
}

func (rp *rootfsProcessor) addBlob(ctx context.Context) error {
	fmt.Println("importing rootfs", rp.rootfs)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()

	gzw := pgzip.NewWriter(writer)
	defer gzw.Close()

	diffID := digest.SHA256.Digester()

	tw := tar.NewWriter(io.MultiWriter(gzw, diffID.Hash()))
	defer tw.Close()

	go func() {
		err := filepath.Walk(rp.rootfs, func(path string, info os.FileInfo, err error) error {
			select {
			case <-ctx.Done():
				return pool.ThreadPoolCancelled
			default:
			}

			if err != nil {
				return err
			}

			var link string
			if info.Mode()&os.ModeSymlink != 0 {
				link, err = os.Readlink(path)
				if err != nil {
					return err
				}
			}

			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}

			hdr.Name = path
			err = tw.WriteHeader(hdr)
			if err != nil {
				return err
			}

			if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()

				n, err := io.Copy(tw, f)
				if err != nil {
					return err
				}

				if n != hdr.Size {
					return fmt.Errorf("Huh? bad size for %s", path)
				}
			}

			return nil
		})
		if err != nil {
			writer.CloseWithError(err)
		}
		tw.Close()
		gzw.Close()
		writer.Close()
	}()

	digest, size, err := rp.oci.PutBlob(reader)
	if err != nil {
		return err
	}

	rp.layerDesc = ispec.Descriptor{
		MediaType: ispec.MediaTypeImageLayerGzip,
		Size:      size,
		Digest:    digest,
	}
	rp.diffID = diffID.Digest()

	return nil
}
