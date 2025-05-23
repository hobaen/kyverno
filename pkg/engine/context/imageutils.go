package context

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/distribution/distribution/reference"
	"github.com/go-logr/logr"
	engineutils "github.com/kyverno/kyverno/pkg/engine/utils"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type ImageInfo struct {

	// Registry is the URL address of the image registry e.g. `docker.io`
	Registry string `json:"registry,omitempty"`

	// Name is the image name portion e.g. `busybox`
	Name string `json:"name"`

	// Path is the repository path and image name e.g. `some-repository/busybox`
	Path string `json:"path"`

	// Tag is the image tag e.g. `v2`
	Tag string `json:"tag,omitempty"`

	// Digest is the image digest portion e.g. `sha256:128c6e3534b842a2eec139999b8ce8aa9a2af9907e2b9269550809d18cd832a3`
	Digest string `json:"digest,omitempty"`

	// JSONPointer is full JSON path to this image e.g. `/spec/containers/0/image`
	JSONPointer string `json:"jsonPath,omitempty"`
}

func (i *ImageInfo) String() string {
	image := i.Registry + "/" + i.Path + ":" + i.Tag
	if i.Digest != "" {
		image = image + "@" + i.Digest
	}

	return image
}

type ContainerImage struct {
	Name  string
	Image *ImageInfo
}

type Images struct {
	InitContainers map[string]*ImageInfo `json:"initContainers,omitempty"`
	Containers     map[string]*ImageInfo `json:"containers"`
}

func newImages(initContainersImgs, containersImgs []*ContainerImage) *Images {
	initContainers := make(map[string]*ImageInfo)
	for _, resource := range initContainersImgs {
		initContainers[resource.Name] = resource.Image
	}

	containers := make(map[string]*ImageInfo)
	for _, resource := range containersImgs {
		containers[resource.Name] = resource.Image
	}

	return &Images{
		InitContainers: initContainers,
		Containers:     containers,
	}
}

func extractImageInfo(resource *unstructured.Unstructured, log logr.Logger) (initContainersImgs, containersImgs []*ContainerImage) {
	logger := log.WithName("extractImageInfo").WithValues("kind", resource.GetKind(), "ns", resource.GetNamespace(), "name", resource.GetName())

	for _, tag := range []string{"initContainers", "containers"} {
		switch resource.GetKind() {
		case "Pod":
			if containers, ok, _ := unstructured.NestedSlice(resource.UnstructuredContent(), "spec", tag); ok {
				if tag == "initContainers" {
					initContainersImgs = extractImageInfos(containers, initContainersImgs, "/spec/initContainers", logger)
				} else {
					containersImgs = extractImageInfos(containers, containersImgs, "/spec/containers", logger)
				}
			}

		case "CronJob":
			if containers, ok, _ := unstructured.NestedSlice(resource.UnstructuredContent(), "spec", "jobTemplate", "spec", "template", "spec", tag); ok {
				if tag == "initContainers" {
					initContainersImgs = extractImageInfos(containers, initContainersImgs, "/spec/jobTemplate/spec/template/spec/initContainers", logger)
				} else {
					containersImgs = extractImageInfos(containers, containersImgs, "/spec/jobTemplate/spec/template/spec/containers", logger)
				}
			}

		// handles "Deployment", "DaemonSet", "Job", "StatefulSet", and custom controllers with the same pattern
		default:
			if containers, ok, _ := unstructured.NestedSlice(resource.UnstructuredContent(), "spec", "template", "spec", tag); ok {
				if tag == "initContainers" {
					initContainersImgs = extractImageInfos(containers, initContainersImgs, "/spec/template/spec/initContainers", logger)
				} else {
					containersImgs = extractImageInfos(containers, containersImgs, "/spec/template/spec/containers", logger)
				}
			}
		}
	}

	return
}

func extractImageInfos(containers []interface{}, images []*ContainerImage, jsonPath string, log logr.Logger) []*ContainerImage {
	img, err := convertToImageInfo(containers, jsonPath)
	if err != nil {
		log.Error(err, "failed to extract image info", "element", containers)
	}

	return append(images, img...)
}

func convertToImageInfo(containers []interface{}, jsonPath string) (images []*ContainerImage, err error) {
	var errs []string
	var index = 0
	for _, ctr := range containers {
		if container, ok := ctr.(map[string]interface{}); ok {
			name := container["name"].(string)
			image := container["image"].(string)
			jp := strings.Join([]string{jsonPath, strconv.Itoa(index), "image"}, "/")
			imageInfo, err := newImageInfo(image, jp)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}

			images = append(images, &ContainerImage{
				Name:  name,
				Image: imageInfo,
			})
		}

		index++
	}

	if len(errs) == 0 {
		return images, nil
	}

	return images, errors.Errorf("%s", strings.Join(errs, ";"))
}

func newImageInfo(image, jsonPointer string) (*ImageInfo, error) {
	image = addDefaultDomain(image)
	ref, err := reference.Parse(image)
	if err != nil {
		return nil, errors.Wrapf(err, "bad image: %s", image)
	}

	var registry, path, name, tag, digest string
	if named, ok := ref.(reference.Named); ok {
		registry = reference.Domain(named)
		path = reference.Path(named)
		name = path[strings.LastIndex(path, "/")+1:]
	}

	if tagged, ok := ref.(reference.Tagged); ok {
		tag = tagged.Tag()
	}

	if digested, ok := ref.(reference.Digested); ok {
		digest = digested.Digest().String()
	}

	// set default tag - the domain is set via addDefaultDomain before parsing
	if tag == "" {
		tag = "latest"
	}

	return &ImageInfo{
		Registry:    registry,
		Name:        name,
		Path:        path,
		Tag:         tag,
		Digest:      digest,
		JSONPointer: jsonPointer,
	}, nil
}

func addDefaultDomain(name string) string {
	i := strings.IndexRune(name, '/')
	if i == -1 || (!strings.ContainsAny(name[:i], ".:") && name[:i] != "localhost" && strings.ToLower(name[:i]) == name[:i]) {
		return "docker.io/" + name
	}

	return name
}

func MutateResourceWithImageInfo(raw []byte, ctx *Context) error {
	images := ctx.ImageInfo()
	if images == nil {
		return nil
	}

	buildJSONPatch := func(op, path, value string) []byte {
		p := fmt.Sprintf(`{ "op": "%s", "path": "%s", "value":"%s" }`, op, path, value)
		return []byte(p)
	}

	var patches [][]byte
	for _, info := range images.Containers {
		patches = append(patches, buildJSONPatch("replace", info.JSONPointer, info.String()))
	}

	for _, info := range images.InitContainers {
		patches = append(patches, buildJSONPatch("replace", info.JSONPointer, info.String()))
	}

	patchedResource, err := engineutils.ApplyPatches(raw, patches)
	if err != nil {
		return err
	}

	return ctx.AddResource(patchedResource)
}
