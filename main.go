package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"
	"github.com/rs/zerolog/log"
	"github.com/samber/lo"
	"pault.ag/go/loopback"
)

type CreateVMResponse struct {
	IPAddress string `json:"ipAddress"`
}

func deleteDirContents(dirName string) error {
	files, err := ioutil.ReadDir(dirName)
	if err != nil {
		return err
	}

	for _, file := range files {
		err := os.RemoveAll(path.Join(dirName, file.Name()))
		if err != nil {
			return err
		}
	}
	return nil
}

/*
buildSquashFSImage's logic, based off https://github.com/firecracker-microvm/firecracker/discussions/3061
1. Mount base image
2. Create directories in image to be used for overlay (https://windsock.io/the-overlay-filesystem/)
  i. 	/overlay/work - scratch space
  ii. 	/overlay/root - upperdir that provides the writeable layer
  iii.	/mnt - the new root
  iv. 	/mnt/rom - the old root
3. Copy overlay_init into /sbin/overlay-init
3. Make squashfs

*/
func buildSquashFSImage(pathToBaseImage string, pathToInitScript string, pathToNewSquashImage string) error {

	mountDir, cleanUp, err := mountImageToRandomDir(pathToBaseImage)
	if err != nil {
		return err

	}
	defer cleanUp()

	// Create directories that will be used later for overlay
	os.MkdirAll(filepath.Join(mountDir, "overlay", "work"), 755)
	os.MkdirAll(filepath.Join(mountDir, "overlay", "root"), 755)
	os.MkdirAll(filepath.Join(mountDir, "mnt"), 755)
	os.MkdirAll(filepath.Join(mountDir, "rom"), 755)

	dirEntries, err := os.ReadDir(filepath.Join(mountDir))
	if err != nil {
		log.Error().Msg("Unable to read directory entries")
		return err
	}

	for _, entry := range dirEntries {
		fmt.Println(entry.Name())
	}

	// Copy overlay_init
	destination, err := os.Create(filepath.Join(mountDir, "sbin", "overlay-init"))
	if err != nil {
		return err
	}
	os.Chmod(destination.Name(), os.FileMode(0755))
	defer destination.Close()

	overlay_init, err := os.ReadFile(filepath.Join(".", pathToInitScript))
	if err != nil {
		return err

	}

	_, err = destination.Write(overlay_init)
	if err != nil {
		return err

	}

	// TODO Use zstd?
	mksquashfs := exec.Command("mksquashfs", mountDir, pathToNewSquashImage, "-noappend")
	err = mksquashfs.Run()
	if err != nil {
		return err

	}

	// List directories
	dirEntries, err = os.ReadDir(filepath.Join(mountDir, "sbin"))

	if err != nil {
		log.Error().Msg("Unable to read directory entries")
		return err

	}

	for _, entry := range dirEntries {
		fmt.Println(entry.Name())
	}

	return nil

}

// mountImageToRandomDir returns the mountpoint
func mountImageToRandomDir(pathToBaseImage string) (string, func(), error) {
	randomDirName, err := os.MkdirTemp("/tmp", "*")
	if err != nil {
		log.Error().Msg("Unable to create random folder")
		return "", func() {}, err
	}
	log.Debug().Msg("Random folder generated=" + randomDirName)

	// Must open as RDWR
	imageFile, err := os.OpenFile(pathToBaseImage, os.O_RDWR, 0)
	if err != nil {
		log.Error().Msg("Unable to open " + pathToBaseImage + " for reading")
		return "", func() {}, err
	}

	log.Debug().Msg("Mounting " + pathToBaseImage)

	_, unmount, err := loopback.MountImage(imageFile, randomDirName, "ext4", 0, "")
	if err != nil {
		log.Error().Err(err).Msg("Unable to mount")
		return "", func() {}, err
	}

	cleanUp := func() {
		log.Debug().Msg("Cleaning up...")
		imageFile.Close()
		unmount()
		os.RemoveAll(randomDirName)
	}

	return randomDirName, cleanUp, nil
}

func main() {

	// Make squashFS image
	const squashFsImage = "./squash-rootfs.img"
	_, err := os.Stat(squashFsImage)
	if errors.Is(err, fs.ErrNotExist) {
		log.Debug().Msg(squashFsImage + "does not exist. Creating...")

		err = buildSquashFSImage("./bionic.rootfs.base.ext4", "./overlay-init", squashFsImage)

		if err != nil {
			log.Error().Err(err).Msgf("Unable to create %s image", squashFsImage)
			os.Exit(1)
		}
	}

	// Setup directory to store socket files
	socketRootDir := "/tmp/firecracker"
	os.MkdirAll(socketRootDir, fs.ModePerm)
	defer deleteDirContents(socketRootDir)

	// Configure API server
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(render.SetContentType(render.ContentTypeJSON))

	r.Get("/vm", func(w http.ResponseWriter, r *http.Request) {
		ipAddr := makeVM(socketRootDir)
		log.Debug().Msgf("IPaddr=%s", ipAddr)
		render.JSON(w, r, CreateVMResponse{IPAddress: ipAddr})
	})

	// Start API server
	const port = 3000
	log.Info().Msgf("Starting API server at %d", port)
	err = http.ListenAndServe(fmt.Sprintf(":%d", port), r)

	if err != nil {
		log.Error().Msg(err.Error())
	}
}

func makeVM(socketDir string) string {

	// Create a unique ID
	rand.Seed(time.Now().Unix())
	id := strconv.Itoa(rand.Intn(10000000))
	sockName := id + ".sock"
	log.Debug().Msgf("Creating uVM and using %s as API socket", sockName)

	// Create logs files
	// TODO Is there a better way to create logs inside logs/ other than pre-creating /logs
	stdout, _ := os.Create("logs/" + id + "-out.log")
	stderr, _ := os.Create("logs/" + id + "-err.log")

	defer stdout.Close()
	defer stderr.Close()

	config := fc.Config{
		SocketPath:      path.Join(socketDir, sockName),
		LogPath:         stdout.Name(),
		LogLevel:        "Info",
		KernelImagePath: "vmlinux.bin",
		KernelArgs:      "console=ttyS0 reboot=k panic=1 pci=off overlay_root=ram ssh_disk=/dev/vdb init=/sbin/overlay-init",
		Drives: []models.Drive{
			{
				DriveID:      lo.ToPtr("rootfs"),
				PathOnHost:   lo.ToPtr("squash-rootfs.img"),
				IsRootDevice: lo.ToPtr(true),
				IsReadOnly:   lo.ToPtr(true),
				CacheType:    lo.ToPtr("Unsafe"),
				IoEngine:     lo.ToPtr("Sync"),
				RateLimiter:  nil,
			}, {
				DriveID:      lo.ToPtr("vol2"),
				PathOnHost:   lo.ToPtr("ext4.img"),
				IsRootDevice: lo.ToPtr(false),
				IsReadOnly:   lo.ToPtr(true),
				CacheType:    lo.ToPtr("Unsafe"),
				IoEngine:     lo.ToPtr("Sync"),
				RateLimiter:  nil,
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:       lo.ToPtr(int64(2)),
			MemSizeMib:      lo.ToPtr(int64(1024)),
			Smt:             lo.ToPtr(false),
			TrackDirtyPages: false,
		},

		NetworkInterfaces: fc.NetworkInterfaces{
			fc.NetworkInterface{
				CNIConfiguration: &firecracker.CNIConfiguration{
					NetworkName: "fcnet",
					IfName:      "veth0",
				},
				AllowMMDS: true,
			},
		},
		// what is MetricsFifo and LogsFifo
	}

	uVM, err := fc.NewMachine(context.Background(), config)
	if err != nil {
		log.Error().Msg(err.Error())
	}

	uVM.Start(context.Background())

	if err != nil {
		log.Error().Msg(err.Error())
	}

	// Get allocated IP address from CNI
	ipBuf, err := ioutil.ReadFile("/var/lib/cni/networks/fcnet/last_reserved_ip.0")
	return string(ipBuf)
}
