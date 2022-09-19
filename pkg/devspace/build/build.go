package build

import (
	"fmt"
	"strings"

	"github.com/loft-sh/devspace/pkg/devspace/build/registry"
	"github.com/loft-sh/devspace/pkg/devspace/build/types"
	"github.com/loft-sh/devspace/pkg/devspace/config/constants"
	"github.com/loft-sh/devspace/pkg/devspace/config/localcache"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	"github.com/loft-sh/devspace/pkg/util/stringutil"

	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	"github.com/loft-sh/devspace/pkg/devspace/hook"
	"github.com/loft-sh/devspace/pkg/util/randutil"
	"github.com/pkg/errors"
)

type imageNameAndTag struct {
	imageConfigName string
	imageName       string
	imageTag        string
	imageTags       []string
	imageConfig     latest.Image
}

// Options describe how images should be build
type Options struct {
	Tags                      []string `long:"tag" short:"t" description:"If enabled will override the default tags"`
	SkipBuild                 bool     `long:"skip" description:"If enabled will skip building"`
	SkipPush                  bool     `long:"skip-push" description:"Skip pushing"`
	SkipPushOnLocalKubernetes bool     `long:"skip-push-on-local-kubernetes" description:"Skip pushing"`
	ForceRebuild              bool     `long:"force-rebuild" description:"Skip pushing"`
	Sequential                bool     `long:"sequential" description:"Skip pushing"`

	MaxConcurrentBuilds int `long:"max-concurrent" description:"A pointer to an integer"`
}

// Controller is the main building interface
type Controller interface {
	Build(ctx devspacecontext.Context, images []string, options *Options) error
}

type controller struct {
	// localRegistry *registry.LocalRegistry
}

// NewController creates a new image build controller
func NewController() Controller {
	return &controller{}
}

// Build builds all images
func (c *controller) Build(ctx devspacecontext.Context, images []string, options *Options) error {
	var (
		builtImages = make(map[string]types.ImageNameTag)

		// Parallel build
		errChan   = make(chan error)
		cacheChan = make(chan imageNameAndTag)
		conf      = ctx.Config().Config()
	)

	// Check if we have at least 1 image to build
	if options.SkipBuild {
		ctx.Log().Debugf("Skip building because of --skip-build")
		return nil
	} else if len(conf.Images) == 0 {
		ctx.Log().Debugf("Skip building because no images are defined")
		return nil
	}

	// Build not in parallel when we only have one image to build
	if !options.Sequential {
		// check if all images are disabled besides one
		imagesToBuild := 0
		for k := range conf.Images {
			if len(images) > 0 && !stringutil.Contains(images, k) {
				continue
			}
			imagesToBuild++
		}
		if len(conf.Images) <= 1 || imagesToBuild <= 1 {
			options.Sequential = true
		}
	}

	imageConfs := map[string]*latest.Image{}
	localRegistries := map[string]*registry.LocalRegistry{}
	for imageConfigName, imageConf := range conf.Images {
		// imageConfCopy := &latest.Image{}
		// data, err := yaml.Marshal(imageConf)
		// if err != nil {
		// 	return err
		// }

		// err = yaml.Unmarshal(data, imageConfCopy)
		// if err != nil {
		// 	return err
		// }

		// Check compatibility with image build
		isLocalRegistryConfigured := imageConf.LocalRegistry != nil
		if isLocalRegistryConfigured && !registry.IsLocalRegistrySupported(imageConf) {
			return fmt.Errorf("local registry is configured for this image build, but is only available for docker and buildkit image builds")
		}

		isLocalReqistryRequired := !registry.HasPushPermission(imageConf)

		// No push permissions and local registry is disabled
		if isLocalReqistryRequired && registry.IsLocalRegistryDisabled(imageConf) {
			return fmt.Errorf("unable to push image %s and using a local registry is disabled", imageConf.Image)
		}

		// No push permissions and local registry is not supported by the build type
		if isLocalReqistryRequired && !registry.IsLocalRegistrySupported(imageConf) {
			return fmt.Errorf("unable to push image %s and only docker and buildkit builds support using a local registry", imageConf.Image)
		}

		if isLocalReqistryRequired {
			options := registry.NewDefaultOptions().
				WithNamespace(ctx.KubeClient().Namespace())

			if imageConf.LocalRegistry != nil {
				options = options.
					WithName(imageConf.LocalRegistry.Name).
					WithNamespace(imageConf.LocalRegistry.Namespace).
					WithImage(imageConf.LocalRegistry.Image).
					WithPort(imageConf.LocalRegistry.Port)

				if imageConf.LocalRegistry.Persistence != nil {
					options = options.
						EnableStorage().
						WithStorageClassName(imageConf.LocalRegistry.Persistence.StorageClassName).
						WithStorageSize(imageConf.LocalRegistry.Persistence.Size)
				}
			}

			localRegistry := localRegistries[options.ID()]
			if localRegistry == nil {
				localRegistry = registry.NewLocalRegistry(options)

				err := localRegistry.Start(ctx)
				if err != nil {
					return errors.Wrap(err, "start registry")
				}

				localRegistries[options.ID()] = localRegistry
			}

			rewrittenImage, err := localRegistry.RewriteImage(imageConf.Image)
			if err != nil {
				return errors.Wrap(err, "rewrite image")
			}

			imageConf.Image = rewrittenImage

			imageCache, _ := ctx.Config().RemoteCache().GetImageCache(imageConfigName)
			imageCache.ImageName = rewrittenImage
			ctx.Config().RemoteCache().SetImageCache(imageConfigName, imageCache)
		}

		imageConfs[imageConfigName] = imageConf
	}

	// Execute before images build hook
	pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{}, "before:build")
	if pluginErr != nil {
		return pluginErr
	}

	imagesToBuild := 0
	for key, imageConf := range imageConfs {
		ctx := ctx.WithLogger(ctx.Log().WithPrefix("build:" + key + " "))
		if len(images) > 0 && !stringutil.Contains(images, key) {
			continue
		}

		// This is necessary for parallel build otherwise we would override the image conf pointer during the loop
		cImageConf := *imageConf
		imageName := cImageConf.Image
		imageConfigName := key

		// Get image tags
		imageTags := []string{}
		if len(options.Tags) > 0 {
			imageTags = append(imageTags, options.Tags...)
		} else if len(imageConf.Tags) > 0 {
			imageTags = append(imageTags, imageConf.Tags...)
		} else {
			imageTags = append(imageTags, randutil.GenerateRandomString(7))
		}

		// replace the # in the tags
		for i := range imageTags {
			for strings.Contains(imageTags[i], "#") {
				imageTags[i] = strings.Replace(imageTags[i], "#", randutil.GenerateRandomString(1), 1)
			}
		}

		// Create new builder
		builder, err := c.createBuilder(ctx, imageConfigName, &cImageConf, imageTags, options)
		if err != nil {
			return errors.Wrap(err, "create builder")
		}

		// Execute before images build hook
		pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{
			"IMAGE_CONFIG_NAME": imageConfigName,
			"IMAGE_NAME":        imageName,
			"IMAGE_CONFIG":      cImageConf,
			"IMAGE_TAGS":        imageTags,
		}, hook.EventsForSingle("before:build", imageConfigName).With("build.beforeBuild")...)
		if pluginErr != nil {
			return pluginErr
		}

		// Check if rebuild is needed
		needRebuild, err := builder.ShouldRebuild(ctx, options.ForceRebuild)
		if err != nil {
			pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{
				"IMAGE_CONFIG_NAME": imageConfigName,
				"IMAGE_NAME":        imageName,
				"IMAGE_CONFIG":      cImageConf,
				"IMAGE_TAGS":        imageTags,
				"ERROR":             err,
			}, hook.EventsForSingle("error:build", imageConfigName).With("build.errorBuild")...)
			if pluginErr != nil {
				return pluginErr
			}
			return errors.Errorf("error during shouldRebuild check: %v", err)
		}

		if !options.ForceRebuild && !needRebuild {
			// Execute before images build hook
			pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{
				"IMAGE_CONFIG_NAME": imageConfigName,
				"IMAGE_NAME":        imageName,
				"IMAGE_CONFIG":      cImageConf,
				"IMAGE_TAGS":        imageTags,
			}, hook.EventsForSingle("skip:build", imageConfigName)...)
			if pluginErr != nil {
				return pluginErr
			}
			ctx.Log().Infof("Skip building image '%s'", imageConfigName)
			continue
		}

		// Sequential or parallel build?
		if options.Sequential {
			// Build the image
			err = builder.Build(ctx)
			if err != nil {
				pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{
					"IMAGE_CONFIG_NAME": imageConfigName,
					"IMAGE_NAME":        imageName,
					"IMAGE_CONFIG":      cImageConf,
					"IMAGE_TAGS":        imageTags,
					"ERROR":             err,
				}, hook.EventsForSingle("error:build", imageConfigName).With("build.errorBuild")...)
				if pluginErr != nil {
					return pluginErr
				}
				return errors.Wrapf(err, "error building image %s:%s", imageName, imageTags[0])
			}

			// Update cache
			ctx.Config().UpdateImageCache(imageConfigName, func(imageCache *localcache.ImageCache) {
				if imageCache.Tag == imageTags[0] {
					ctx.Log().Warnf("Newly built image '%s' has the same tag as in the last build (%s), this can lead to problems that the image during deployment is not updated", imageName, imageTags[0])
				}

				imageCache.ImageName = imageName
				imageCache.Tag = imageTags[0]
			})

			// Track built images
			builtImages[imageConfigName] = types.ImageNameTag{
				ImageConfigName: imageConfigName,
				ImageName:       imageName,
				ImageTag:        imageTags[0],
			}

			// Execute before images build hook
			pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{
				"IMAGE_CONFIG_NAME": imageConfigName,
				"IMAGE_NAME":        imageName,
				"IMAGE_CONFIG":      cImageConf,
				"IMAGE_TAGS":        imageTags,
			}, hook.EventsForSingle("after:build", imageConfigName).With("build.afterBuild")...)
			if pluginErr != nil {
				return pluginErr
			}
		} else {
			// wait until we are below the MaxConcurrency
			if options.MaxConcurrentBuilds > 0 && imagesToBuild >= options.MaxConcurrentBuilds {
				err = c.waitForBuild(ctx, errChan, cacheChan, builtImages)
				if err != nil {
					return err
				}

				imagesToBuild--
			}

			imagesToBuild++
			go func(ctx devspacecontext.Context) {
				// Build the image
				err := builder.Build(ctx)
				if err != nil {
					hook.LogExecuteHooks(ctx, map[string]interface{}{
						"IMAGE_CONFIG_NAME": imageConfigName,
						"IMAGE_NAME":        imageName,
						"IMAGE_CONFIG":      cImageConf,
						"IMAGE_TAGS":        imageTags,
						"ERROR":             err,
					}, hook.EventsForSingle("error:build", imageConfigName).With("build.errorBuild")...)
					errChan <- errors.Errorf("error building image %s:%s: %v", imageName, imageTags[0], err)
					return
				}

				// Send the response
				cacheChan <- imageNameAndTag{
					imageConfigName: imageConfigName,
					imageName:       imageName,
					imageTag:        imageTags[0],
					imageTags:       imageTags,
					imageConfig:     cImageConf,
				}
			}(ctx)
		}
	}

	// wait for the builds to finish
	if !options.Sequential {
		for imagesToBuild > 0 {
			err := c.waitForBuild(ctx, errChan, cacheChan, builtImages)
			if err != nil {
				return err
			}

			imagesToBuild--
		}
	}

	// Execute after images build hook
	pluginErr = hook.ExecuteHooks(ctx, map[string]interface{}{}, "after:build")
	if pluginErr != nil {
		return pluginErr
	}

	// merge built images
	alreadyBuiltImages, ok := ctx.Config().GetRuntimeVariable(constants.BuiltImagesKey)
	if ok {
		alreadyBuiltImagesMap, ok := alreadyBuiltImages.(map[string]types.ImageNameTag)
		if ok {
			for k, v := range alreadyBuiltImagesMap {
				_, ok := builtImages[k]
				if ok {
					continue
				}

				builtImages[k] = v
			}
		}
	}

	ctx.Config().SetRuntimeVariable(constants.BuiltImagesKey, builtImages)

	if len(builtImages) > 0 {
		err := ctx.Config().LocalCache().Save()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *controller) waitForBuild(ctx devspacecontext.Context, errChan <-chan error, cacheChan <-chan imageNameAndTag, builtImages map[string]types.ImageNameTag) error {
	select {
	case err := <-errChan:
		return err
	case done := <-cacheChan:
		ctx := ctx.WithLogger(ctx.Log().WithPrefix("build:" + done.imageConfigName + " "))
		ctx.Log().Donef("Done building image %s:%s (%s)", done.imageName, done.imageTag, done.imageConfigName)

		// Update cache
		ctx.Config().UpdateImageCache(done.imageConfigName, func(imageCache *localcache.ImageCache) {
			if imageCache.Tag == done.imageTag {
				ctx.Log().Warnf("Newly built image '%s' has the same tag as in the last build (%s), this can lead to problems that the image during deployment is not updated", done.imageName, done.imageTag)
			}

			imageCache.ImageName = done.imageName
			imageCache.Tag = done.imageTag
		})

		// Track built images
		builtImages[done.imageConfigName] = types.ImageNameTag{
			ImageConfigName: done.imageConfigName,
			ImageName:       done.imageName,
			ImageTag:        done.imageTag,
		}

		// Execute plugin hook
		pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{
			"IMAGE_CONFIG_NAME": done.imageConfigName,
			"IMAGE_NAME":        done.imageName,
			"IMAGE_CONFIG":      done.imageConfig,
			"IMAGE_TAGS":        done.imageTags,
		}, hook.EventsForSingle("after:build", done.imageConfigName).With("build.afterBuild")...)
		if pluginErr != nil {
			return pluginErr
		}
	}

	return nil
}
