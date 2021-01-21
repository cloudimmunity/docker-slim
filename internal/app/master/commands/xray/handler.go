package xray

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker-slim/docker-slim/internal/app/master/commands"
	"github.com/docker-slim/docker-slim/internal/app/master/docker/dockerclient"
	"github.com/docker-slim/docker-slim/internal/app/master/inspectors/image"
	"github.com/docker-slim/docker-slim/internal/app/master/version"
	"github.com/docker-slim/docker-slim/pkg/command"
	"github.com/docker-slim/docker-slim/pkg/docker/dockerimage"
	"github.com/docker-slim/docker-slim/pkg/docker/dockerutil"
	"github.com/docker-slim/docker-slim/pkg/report"
	"github.com/docker-slim/docker-slim/pkg/util/errutil"
	"github.com/docker-slim/docker-slim/pkg/util/fsutil"
	v "github.com/docker-slim/docker-slim/pkg/version"

	"github.com/dustin/go-humanize"
	log "github.com/sirupsen/logrus"
)

const appName = commands.AppName

// Xray command exit codes
const (
	ecxOther = iota + 1
	ecxImageNotFound
)

// OnCommand implements the 'xray' docker-slim command
func OnCommand(
	gparams *commands.GenericParams,
	targetRef string,
	changes map[string]struct{},
	layers map[string]struct{},
	doAddImageManifest bool,
	doAddImageConfig bool,
	doRmFileArtifacts bool,
	ec *commands.ExecutionContext) {
	const cmdName = Name
	logger := log.WithFields(log.Fields{"app": appName, "command": cmdName})
	prefix := fmt.Sprintf("cmd=%s", cmdName)

	viChan := version.CheckAsync(gparams.CheckVersion, gparams.InContainer, gparams.IsDSImage)

	cmdReport := report.NewXrayCommand(gparams.ReportLocation, gparams.InContainer)
	cmdReport.State = command.StateStarted
	cmdReport.TargetReference = targetRef

	fmt.Printf("cmd=%s state=started\n", cmdName)
	fmt.Printf("cmd=%s info=params target=%v add-image-manifest=%v add-image-config=%v rm-file-artifacts=%v\n",
		cmdName, targetRef, doAddImageManifest, doAddImageConfig, doRmFileArtifacts)

	client, err := dockerclient.New(gparams.ClientConfig)
	if err == dockerclient.ErrNoDockerInfo {
		exitMsg := "missing Docker connection info"
		if gparams.InContainer && gparams.IsDSImage {
			exitMsg = "make sure to pass the Docker connect parameters to the docker-slim container"
		}
		fmt.Printf("cmd=%s info=docker.connect.error message='%s'\n", cmdName, exitMsg)
		fmt.Printf("cmd=%s state=exited version=%s location='%s'\n", cmdName, v.Current(), fsutil.ExeDir())
		commands.Exit(commands.ECTCommon | commands.ECNoDockerConnectInfo)
	}
	errutil.FailOn(err)

	if gparams.Debug {
		version.Print(prefix, logger, client, false, gparams.InContainer, gparams.IsDSImage)
	}

	imageInspector, err := image.NewInspector(client, targetRef)
	errutil.FailOn(err)

	if imageInspector.NoImage() {
		fmt.Printf("cmd=%s info=target.image.error status=not.found image='%v' message='make sure the target image already exists locally'\n", cmdName, targetRef)
		fmt.Printf("cmd=%s state=exited\n", cmdName)
		commands.Exit(commands.ECTBuild | ecxImageNotFound)
	}

	fmt.Printf("cmd=%s state=image.api.inspection.start\n", cmdName)

	logger.Info("inspecting 'fat' image metadata...")
	err = imageInspector.Inspect()
	errutil.FailOn(err)

	localVolumePath, artifactLocation, statePath, stateKey := fsutil.PrepareImageStateDirs(gparams.StatePath, imageInspector.ImageInfo.ID)
	imageInspector.ArtifactLocation = artifactLocation
	logger.Debugf("localVolumePath=%v, artifactLocation=%v, statePath=%v, stateKey=%v", localVolumePath, artifactLocation, statePath, stateKey)

	fmt.Printf("cmd=%s info=image id=%v size.bytes=%v size.human='%v'\n",
		cmdName,
		imageInspector.ImageInfo.ID,
		imageInspector.ImageInfo.VirtualSize,
		humanize.Bytes(uint64(imageInspector.ImageInfo.VirtualSize)))

	logger.Info("processing 'fat' image info...")
	err = imageInspector.ProcessCollectedData()
	errutil.FailOn(err)

	if imageInspector.DockerfileInfo != nil {
		if imageInspector.DockerfileInfo.ExeUser != "" {
			fmt.Printf("cmd=%s info=image.users exe='%v' all='%v'\n",
				cmdName,
				imageInspector.DockerfileInfo.ExeUser,
				strings.Join(imageInspector.DockerfileInfo.AllUsers, ","))
		}

		if len(imageInspector.DockerfileInfo.ImageStack) > 0 {
			cmdReport.ImageStack = imageInspector.DockerfileInfo.ImageStack

			for idx, imageInfo := range imageInspector.DockerfileInfo.ImageStack {
				fmt.Printf("cmd=%s info=image.stack index=%v name='%v' id='%v' instructions=%v message='see report file for details'\n",
					cmdName, idx, imageInfo.FullName, imageInfo.ID, len(imageInfo.Instructions))
			}
		}

		if len(imageInspector.DockerfileInfo.ExposedPorts) > 0 {
			fmt.Printf("cmd=%s info=image.exposed_ports list='%v'\n", cmdName,
				strings.Join(imageInspector.DockerfileInfo.ExposedPorts, ","))
		}
	}

	cmdReport.SourceImage = report.ImageMetadata{
		AllNames:      imageInspector.ImageRecordInfo.RepoTags,
		ID:            imageInspector.ImageRecordInfo.ID,
		Size:          imageInspector.ImageInfo.VirtualSize,
		SizeHuman:     humanize.Bytes(uint64(imageInspector.ImageInfo.VirtualSize)),
		CreateTime:    imageInspector.ImageInfo.Created.UTC().Format(time.RFC3339),
		Author:        imageInspector.ImageInfo.Author,
		DockerVersion: imageInspector.ImageInfo.DockerVersion,
		Architecture:  imageInspector.ImageInfo.Architecture,
		User:          imageInspector.ImageInfo.Config.User,
	}

	if len(imageInspector.ImageRecordInfo.RepoTags) > 0 {
		cmdReport.SourceImage.Name = imageInspector.ImageRecordInfo.RepoTags[0]
	}

	if len(imageInspector.ImageInfo.Config.ExposedPorts) > 0 {
		for k := range imageInspector.ImageInfo.Config.ExposedPorts {
			cmdReport.SourceImage.ExposedPorts = append(cmdReport.SourceImage.ExposedPorts, string(k))
		}
	}

	cmdReport.ArtifactLocation = imageInspector.ArtifactLocation

	fmt.Printf("cmd=%s state=image.api.inspection.done\n", cmdName)
	fmt.Printf("cmd=%s state=image.data.inspection.start\n", cmdName)

	imageID := dockerutil.CleanImageID(imageInspector.ImageInfo.ID)
	iaName := fmt.Sprintf("%s.tar", imageID)
	iaPath := filepath.Join(localVolumePath, "image", iaName)
	err = dockerutil.SaveImage(client, imageID, iaPath, false, false)
	errutil.FailOn(err)

	imagePkg, err := dockerimage.LoadPackage(iaPath, imageID, false)
	errutil.FailOn(err)

	fmt.Printf("cmd=%s state=image.data.inspection.done\n", cmdName)

	if len(imageInspector.DockerfileInfo.AllInstructions) == len(imagePkg.Config.History) {
		for instIdx, instInfo := range imageInspector.DockerfileInfo.AllInstructions {
			instInfo.Author = imagePkg.Config.History[instIdx].Author
			instInfo.EmptyLayer = imagePkg.Config.History[instIdx].EmptyLayer
			instInfo.LayerID = imagePkg.Config.History[instIdx].LayerID
			instInfo.LayerIndex = imagePkg.Config.History[instIdx].LayerIndex
			instInfo.LayerFSDiffID = imagePkg.Config.History[instIdx].LayerFSDiffID
		}
	} else {
		logger.Debugf("history instruction set size mismatch - %v/%v ",
			len(imageInspector.DockerfileInfo.AllInstructions),
			len(imagePkg.Config.History))
	}

	printImagePackage(imagePkg, appName, cmdName, changes, layers, cmdReport)

	if doAddImageManifest {
		cmdReport.RawImageManifest = imagePkg.Manifest
	}

	if doAddImageConfig {
		cmdReport.RawImageConfig = imagePkg.Config
	}

	fmt.Printf("cmd=%s state=completed\n", cmdName)
	cmdReport.State = command.StateCompleted

	if doRmFileArtifacts {
		logger.Info("removing temporary artifacts...")
		err = fsutil.Remove(iaPath)
		errutil.WarnOn(err)
	} else {
		cmdReport.ImageArchiveLocation = iaPath
	}

	fmt.Printf("cmd=%s state=done\n", cmdName)

	fmt.Printf("cmd=%s info=results  artifacts.location='%v'\n", cmdName, cmdReport.ArtifactLocation)
	fmt.Printf("cmd=%s info=results  artifacts.dockerfile.original=Dockerfile.fat\n", cmdName)

	vinfo := <-viChan
	version.PrintCheckVersion(prefix, vinfo)

	cmdReport.State = command.StateDone
	if cmdReport.Save() {
		fmt.Printf("cmd=%s info=report file='%s'\n", cmdName, cmdReport.ReportLocation())
	}
}

func printImagePackage(pkg *dockerimage.Package,
	appName string,
	cmdName command.Type,
	changes map[string]struct{},
	layers map[string]struct{},
	cmdReport *report.XrayCommand) {
	fmt.Printf("cmd=%s info=image.package.details\n", cmdName)

	fmt.Printf("cmd=%s info=layers.count: %v\n", cmdName, len(pkg.Layers))
	for _, layer := range pkg.Layers {
		fmt.Printf("cmd=%s info=layer index=%d id=%s path=%s\n", cmdName, layer.Index, layer.ID, layer.Path)

		if layer.Stats.AllSize != 0 {
			fmt.Printf("cmd=%s info=layer.stats all_size.human='%v' all_size.bytes=%v\n",
				cmdName, humanize.Bytes(uint64(layer.Stats.AllSize)), layer.Stats.AllSize)
		}

		if layer.Stats.ObjectCount != 0 {
			fmt.Printf("cmd=%s info=layer.stats object_count=%v\n", cmdName, layer.Stats.ObjectCount)
		}

		if layer.Stats.DirCount != 0 {
			fmt.Printf("cmd=%s info=layer.stats dir_count=%v\n", cmdName, layer.Stats.DirCount)
		}

		if layer.Stats.FileCount != 0 {
			fmt.Printf("cmd=%s info=layer.stats file_count=%v\n", cmdName, layer.Stats.FileCount)
		}

		if layer.Stats.LinkCount != 0 {
			fmt.Printf("cmd=%s info=layer.stats link_count=%v\n", cmdName, layer.Stats.LinkCount)
		}

		if layer.Stats.MaxFileSize != 0 {
			fmt.Printf("cmd=%s info=layer.stats max_file_size.human='%v' max_file_size.bytes=%v\n",
				cmdName, humanize.Bytes(uint64(layer.Stats.MaxFileSize)), layer.Stats.MaxFileSize)
		}

		if layer.Stats.DeletedCount != 0 {
			fmt.Printf("cmd=%s info=layer.stats deleted_count=%v\n", cmdName, layer.Stats.DeletedCount)
		}

		if layer.Stats.DeletedDirCount != 0 {
			fmt.Printf("cmd=%s info=layer.stats deleted_dir_count=%v\n", cmdName, layer.Stats.DeletedDirCount)
		}

		if layer.Stats.DeletedFileCount != 0 {
			fmt.Printf("cmd=%s info=layer.stats deleted_file_count=%v\n", cmdName, layer.Stats.DeletedFileCount)
		}

		if layer.Stats.DeletedLinkCount != 0 {
			fmt.Printf("cmd=%s info=layer.stats deleted_link_count=%v\n", cmdName, layer.Stats.DeletedLinkCount)
		}

		if layer.Stats.DeletedSize != 0 {
			fmt.Printf("cmd=%s info=layer.stats deleted_size=%v\n", cmdName, layer.Stats.DeletedSize)
		}

		if layer.Stats.AddedSize != 0 {
			fmt.Printf("cmd=%s info=layer.stats added_size.human='%v' added_size.bytes=%v\n",
				cmdName, humanize.Bytes(uint64(layer.Stats.AddedSize)), layer.Stats.AddedSize)
		}

		if layer.Stats.ModifiedSize != 0 {
			fmt.Printf("cmd=%s info=layer.stats modified_size.human='%v' modified_size.bytes=%v\n",
				cmdName, humanize.Bytes(uint64(layer.Stats.ModifiedSize)), layer.Stats.ModifiedSize)
		}

		changeCount := len(layer.Changes.Deleted) + len(layer.Changes.Modified) + len(layer.Changes.Added)

		fmt.Printf("cmd=%s info=layer.change.summary deleted=%d modified=%d added=%d all=%d\n",
			cmdName, len(layer.Changes.Deleted), len(layer.Changes.Modified),
			len(layer.Changes.Added), changeCount)

		fmt.Printf("cmd=%s info=layer.objects.count data=%d\n", cmdName, len(layer.Objects))

		topList := layer.Top.List()
		fmt.Printf("cmd=%s info=layer.objects.top:\n", cmdName)
		for _, object := range topList {
			printObject(object)
		}

		layerReport := dockerimage.LayerReport{
			ID:       layer.ID,
			Index:    layer.Index,
			Path:     layer.Path,
			FSDiffID: layer.FSDiffID,
			Stats:    layer.Stats,
		}

		layerReport.Changes.Deleted = uint64(len(layer.Changes.Deleted))
		layerReport.Changes.Modified = uint64(len(layer.Changes.Modified))
		layerReport.Changes.Added = uint64(len(layer.Changes.Added))

		layerReport.Top = topList

		cmdReport.ImageLayers = append(cmdReport.ImageLayers, &layerReport)

		fmt.Printf("\n")

		showLayer := true
		if len(layers) > 0 {
			showLayer = false
			_, hasID := layers[layer.ID]
			layerIdx := fmt.Sprintf("%v", layer.Index)
			_, hasIndex := layers[layerIdx]
			if hasID || hasIndex {
				showLayer = true
			}
		}

		if showLayer {
			if _, ok := changes["delete"]; ok && len(layer.Changes.Deleted) > 0 {
				fmt.Printf("cmd=%s info=layer.objects.deleted:\n", cmdName)
				for _, objectIdx := range layer.Changes.Deleted {
					layerReport.Deleted = append(layerReport.Deleted, layer.Objects[objectIdx])
					printObject(layer.Objects[objectIdx])
				}
				fmt.Printf("\n")
			}
			if _, ok := changes["modify"]; ok && len(layer.Changes.Modified) > 0 {
				fmt.Printf("cmd=%s info=layer.objects.modified:\n", cmdName)
				for _, objectIdx := range layer.Changes.Modified {
					layerReport.Modified = append(layerReport.Modified, layer.Objects[objectIdx])
					printObject(layer.Objects[objectIdx])
				}
				fmt.Printf("\n")
			}
			if _, ok := changes["add"]; ok && len(layer.Changes.Added) > 0 {
				fmt.Printf("cmd=%s info=layer.objects.added:\n", cmdName)
				for _, objectIdx := range layer.Changes.Added {
					layerReport.Added = append(layerReport.Added, layer.Objects[objectIdx])
					printObject(layer.Objects[objectIdx])
				}
			}
		}
	}
}

func printObject(object *dockerimage.ObjectMetadata) {
	fmt.Printf("%s: mode=%s size.human='%v' size.bytes=%d uid=%d gid=%d mtime='%s' '%s'",
		object.Change,
		object.Mode,
		humanize.Bytes(uint64(object.Size)),
		object.Size,
		object.UID,
		object.GID,
		object.ModTime.UTC().Format(time.RFC3339),
		object.Name)

	if object.LinkTarget != "" {
		fmt.Printf(" -> '%s'\n", object.LinkTarget)
	} else {
		fmt.Printf("\n")
	}
}
