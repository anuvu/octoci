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
	"github.com/openSUSE/umoci/oci/casext"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/anuvu/octoci/pool"
	"github.com/urfave/cli"
	"github.com/pkg/errors"
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
		os.Exit(1)
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
		cli.IntFlag{
			Name:  "dirs-per-blob",
			Usage: "the number of directories to combine into one layer",
			Value: 1,
		},
		cli.BoolFlag{
			Name: "serialize",
			Usage: "serialize the builds (i.e. don't do them in parallel)",
			Hidden: true,
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
	defer oci.GC(context.Background())
	defer oci.Close()

	tasks := []rootfsProcessor{}
	procs := runtime.NumCPU()
	if ctx.Bool("serialize") {
		procs = 1
	}
	tp := pool.New(procs)

	for i, rootfs := range rootfses {
		if i % ctx.Int("dirs-per-blob") == 0 {
			tasks = append(tasks, rootfsProcessor{oci: oci, rootfses: []string{}})
		}
		rootfs, err = filepath.Abs(rootfs)
		if err != nil {
			return err
		}
		tasks[len(tasks)-1].rootfses = append(tasks[len(tasks)-1].rootfses, rootfs)

	}

	for i, _ := range tasks {
		tp.Add((&tasks[i]).addBlob)
	}

	fmt.Println("done adding jobs")
	tp.DoneAddingJobs()

	err = tp.Run()
	if err != nil {
		return err
	}

	descriptorPaths, err := oci.ResolveReference(context.Background(), ctx.String("tag"))
	if err != nil {
		return err
	}

	if len(descriptorPaths) != 1 {
		return errors.Errorf("bad tag: %s", ctx.String("tag"))
	}

	manifestBlob, err := oci.FromDescriptor(context.Background(), descriptorPaths[0].Descriptor())
	if err != nil {
		return err
	}

	manifest, ok := manifestBlob.Data.(ispec.Manifest)
	if !ok {
		return errors.Errorf("bad manifest data type %T", manifestBlob.Data)
	}

	configBlob, err := oci.FromDescriptor(context.Background(), manifest.Config)
	if err != nil {
		return err
	}

	config, ok := configBlob.Data.(ispec.Image)
	if !ok {
		return errors.Errorf("bad config data type %T", manifestBlob.Data)
	}

	for _, task := range tasks {
		config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, task.diffID)
		manifest.Layers = append(manifest.Layers, task.layerDesc)
	}

	digest, size, err := oci.PutBlobJSON(context.Background(), config)
	if err != nil {
		return err
	}

	manifest.Config = ispec.Descriptor{
		MediaType: ispec.MediaTypeImageConfig,
		Digest:    digest,
		Size:      size,
	}

	digest, size, err = oci.PutBlobJSON(context.Background(), manifest)
	if err != nil {
		return err
	}

	err = oci.UpdateReference(context.Background(), ctx.String("tag"), ispec.Descriptor{
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
	oci       casext.Engine
	rootfses  []string
	diffID    digest.Digest
	layerDesc ispec.Descriptor
}

func (rp *rootfsProcessor) addBlob(ctx context.Context) error {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()

	gzw := pgzip.NewWriter(writer)
	gzw.SetConcurrency(250000, 2 * runtime.NumCPU())
	defer gzw.Close()

	diffID := digest.SHA256.Digester()

	tw := tar.NewWriter(io.MultiWriter(gzw, diffID.Hash()))
	defer tw.Close()

	go func() {

		for _, rootfs := range rp.rootfses {
			handler := func(path string, info os.FileInfo, err error) error {
				select {
				case <-ctx.Done():
					return pool.ThreadPoolCancelled
				default:
				}

				if err != nil {
					return err
				}

				/* don't import an empty path */
				if path == rootfs {
					return nil
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

				hdr.Name = path[len(rootfs):]
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
			}

			fmt.Println("importing rootfs", rootfs)
			err := filepath.Walk(rootfs, handler)
			if err != nil {
				writer.CloseWithError(err)
			}
		}

		tw.Close()
		gzw.Close()
		writer.Close()
	}()

	digest, size, err := rp.oci.PutBlob(context.Background(), reader)
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
