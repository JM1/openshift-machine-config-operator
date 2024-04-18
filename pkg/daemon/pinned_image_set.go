package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/pkg/sysregistriesv2"
	"github.com/containers/image/v5/types"
	ign3types "github.com/coreos/ignition/v2/config/v3_4/types"
	"github.com/golang/groupcache/lru"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformersv1 "k8s.io/client-go/informers/core/v1"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	mcfgv1alpha1 "github.com/openshift/api/machineconfiguration/v1alpha1"
	machineconfigurationalphav1 "github.com/openshift/client-go/machineconfiguration/applyconfigurations/machineconfiguration/v1alpha1"
	mcfgclientset "github.com/openshift/client-go/machineconfiguration/clientset/versioned"
	mcfginformersv1 "github.com/openshift/client-go/machineconfiguration/informers/externalversions/machineconfiguration/v1"
	mcfginformersv1alpha1 "github.com/openshift/client-go/machineconfiguration/informers/externalversions/machineconfiguration/v1alpha1"
	mcfglistersv1 "github.com/openshift/client-go/machineconfiguration/listers/machineconfiguration/v1"
	mcfglistersv1alpha1 "github.com/openshift/client-go/machineconfiguration/listers/machineconfiguration/v1alpha1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	"github.com/openshift/machine-config-operator/pkg/daemon/constants"
	"github.com/openshift/machine-config-operator/pkg/daemon/cri"
	"github.com/openshift/machine-config-operator/pkg/helpers"
	"github.com/openshift/machine-config-operator/pkg/upgrademonitor"
)

const (
	// cri gRPC connection parameters taken from kubelet
	criPrefetchInterval             = 30 * time.Second
	defaultPrefetchWorkers          = 5
	defaultControlPlaneWorkers      = 1
	defaultPrefetchThrottleDuration = 1 * time.Second

	crioPinnedImagesDropInFilePath = "/etc/crio/crio.conf.d/50-pinned-images"

	// backoff configuration
	maxRetries    = 5
	retryDuration = 1 * time.Second
	retryFactor   = 2.0
	retryCap      = 10 * time.Second

	// controller configuration
	maxRetriesController = 15
)

var (
	errInsufficientStorage = errors.New("storage available is less than minimum required")
	errFailedToPullImage   = errors.New("failed to pull image")
	errNotFound            = errors.New("not found")
	errNoAuthForImage      = errors.New("no auth found for image")
)

// PinnedImageSetManager manages the prefetching of images.
type PinnedImageSetManager struct {
	// nodeName is the name of the node.
	nodeName string

	imageSetLister mcfglistersv1alpha1.PinnedImageSetLister
	imageSetSynced cache.InformerSynced

	nodeLister       corev1lister.NodeLister
	nodeListerSynced cache.InformerSynced

	mcpLister mcfglistersv1.MachineConfigPoolLister
	mcpSynced cache.InformerSynced

	mcfgClient mcfgclientset.Interface

	prefetchCh chan prefetch

	criClient *cri.Client

	// minimum storage available after prefetching
	minStorageAvailableBytes resource.Quantity
	// path to the authfile
	authFilePath string
	// path to the registry config file
	registryCfgPath string
	// endpoint of the container runtime service
	runtimeEndpoint string
	// timeout for the prefetch operation
	prefetchTimeout time.Duration
	// backoff configuration for retries.
	backoff wait.Backoff

	// cache for reusable image information
	cache *imageCache

	syncHandler              func(string) error
	enqueueMachineConfigPool func(*mcfgv1.MachineConfigPool)
	queue                    workqueue.RateLimitingInterface
	featureGatesAccessor     featuregates.FeatureGateAccess

	// mutex to protect the cancelFn
	mu       sync.Mutex
	cancelFn context.CancelFunc
}

// NewPinnedImageSetManager creates a new pinned image set manager.
func NewPinnedImageSetManager(
	nodeName string,
	criClient *cri.Client,
	mcfgClient mcfgclientset.Interface,
	imageSetInformer mcfginformersv1alpha1.PinnedImageSetInformer,
	nodeInformer coreinformersv1.NodeInformer,
	mcpInformer mcfginformersv1.MachineConfigPoolInformer,
	minStorageAvailableBytes resource.Quantity,
	runtimeEndpoint,
	authFilePath,
	registryCfgPath string,
	prefetchTimeout time.Duration,
	featureGatesAccessor featuregates.FeatureGateAccess,
) *PinnedImageSetManager {
	p := &PinnedImageSetManager{
		nodeName:                 nodeName,
		mcfgClient:               mcfgClient,
		runtimeEndpoint:          runtimeEndpoint,
		authFilePath:             authFilePath,
		registryCfgPath:          registryCfgPath,
		prefetchTimeout:          prefetchTimeout,
		minStorageAvailableBytes: minStorageAvailableBytes,
		featureGatesAccessor:     featureGatesAccessor,
		queue:                    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "pinned-image-set-manager"),
		prefetchCh:               make(chan prefetch, defaultPrefetchWorkers*2),
		criClient:                criClient,
		backoff: wait.Backoff{
			Steps:    maxRetries,
			Duration: retryDuration,
			Factor:   retryFactor,
			Cap:      retryCap,
		},
	}

	imageSetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    p.addPinnedImageSet,
		UpdateFunc: p.updatePinnedImageSet,
		DeleteFunc: p.deletePinnedImageSet,
	})

	mcpInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    p.addMachineConfigPool,
		UpdateFunc: p.updateMachineConfigPool,
		DeleteFunc: p.deleteMachineConfigPool,
	})

	p.syncHandler = p.sync
	p.enqueueMachineConfigPool = p.enqueue

	p.imageSetLister = imageSetInformer.Lister()
	p.imageSetSynced = imageSetInformer.Informer().HasSynced

	p.nodeLister = nodeInformer.Lister()
	p.nodeListerSynced = nodeInformer.Informer().HasSynced

	p.mcpLister = mcpInformer.Lister()
	p.mcpSynced = mcpInformer.Informer().HasSynced

	p.cache = newImageCache(256)

	return p
}

func (p *PinnedImageSetManager) sync(key string) error {
	klog.V(4).Infof("Syncing PinnedImageSet for pool: %s", key)
	node, err := p.nodeLister.Get(p.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %q: %w", p.nodeName, err)
	}

	pools, _, err := helpers.GetPoolsForNode(p.mcpLister, node)
	if err != nil {
		return err
	}

	if len(pools) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.prefetchTimeout)
	// cancel any currently running tasks in the worker pool
	p.resetWorkload(cancel)

	if err := p.syncMachineConfigPools(ctx, pools); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			ctxErr := fmt.Errorf("requeue: prefetching images incomplete after: %v", p.prefetchTimeout)
			p.updateStatusError(pools, ctxErr)
			klog.Info(ctxErr)
			return ctxErr
		}

		klog.Errorf("Failed to prefetch and pin images: %v", err)
		p.updateStatusError(pools, err)
		return err
	}

	p.updateStatusProgressingComplete(pools, "All pinned image sets complete")
	return nil
}

func (p *PinnedImageSetManager) syncMachineConfigPools(ctx context.Context, pools []*mcfgv1.MachineConfigPool) error {
	images := make([]mcfgv1alpha1.PinnedImageRef, 0, 100)
	for _, pool := range pools {
		if err := p.syncMachineConfigPool(ctx, pool); err != nil {
			return err
		}

		// collect all unique images from all pools
		for _, image := range pool.Spec.PinnedImageSets {
			imageSet, err := p.imageSetLister.Get(image.Name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					klog.Warningf("PinnedImageSet %q not found", image.Name)
					continue
				}
				return fmt.Errorf("failed to get PinnedImageSet %q: %w", image.Name, err)
			}
			images = append(images, imageSet.Spec.PinnedImages...)
		}
	}

	// verify all images available if not clear the cache and requeue
	imageNames := uniqueSortedImageNames(images)
	for _, image := range imageNames {
		exists, err := p.criClient.ImageStatus(ctx, image)
		if err != nil {
			return err
		}
		if !exists {
			p.cache.Clear()
			return errFailedToPullImage
		}
	}

	// write config and reload crio last to allow a window for kubelet to gc
	// images in an emergency
	if err := ensureCrioPinnedImagesConfigFile(crioPinnedImagesDropInFilePath, imageNames); err != nil {
		klog.Errorf("failed to write crio config file: %v", err)
		return err
	}

	return nil
}

func (p *PinnedImageSetManager) syncMachineConfigPool(ctx context.Context, pool *mcfgv1.MachineConfigPool) error {
	if pool.Spec.PinnedImageSets == nil {
		return nil
	}
	imageSets := make([]*mcfgv1alpha1.PinnedImageSet, 0, len(pool.Spec.PinnedImageSets))
	for _, ref := range pool.Spec.PinnedImageSets {
		imageSet, err := p.imageSetLister.Get(ref.Name)
		if err != nil {
			return fmt.Errorf("failed to get PinnedImageSet %q: %w", ref.Name, err)
		}
		klog.Infof("Reconciling pinned image set: %s: generation: %d", ref.Name, imageSet.GetGeneration())

		// verify storage per image set
		if err := p.checkNodeAllocatableStorage(ctx, imageSet); err != nil {
			return err
		}
		imageSets = append(imageSets, imageSet)
	}

	return p.prefetchImageSets(ctx, imageSets...)
}

func (p *PinnedImageSetManager) checkNodeAllocatableStorage(ctx context.Context, imageSet *mcfgv1alpha1.PinnedImageSet) error {
	node, err := p.nodeLister.Get(p.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %q: %w", p.nodeName, err)
	}

	storageCapacity, ok := node.Status.Allocatable[corev1.ResourceEphemeralStorage]
	if !ok {
		return fmt.Errorf("node %q has no ephemeral storage capacity", p.nodeName)
	}

	capacity := storageCapacity.Value()
	if storageCapacity.Cmp(p.minStorageAvailableBytes) < 0 {
		return fmt.Errorf("%w capacity: %d, required: %d", errInsufficientStorage, capacity, p.minStorageAvailableBytes.Value())
	}

	return p.checkImagePayloadStorage(ctx, imageSet.Spec.PinnedImages, capacity)
}

// prefetchImageSets schedules the prefetching of images for the given image sets and waits for completion.
func (p *PinnedImageSetManager) prefetchImageSets(ctx context.Context, imageSets ...*mcfgv1alpha1.PinnedImageSet) error {
	registryAuth, err := newRegistryAuth(p.authFilePath, p.registryCfgPath)
	if err != nil {
		return err
	}

	// monitor prefetch operations
	monitor := newPrefetchMonitor()
	for _, imageSet := range imageSets {
		// this is forbidden by the API validation rules, but we should check anyway
		if len(imageSet.Spec.PinnedImages) == 0 {
			continue
		}

		cachedImage, ok := p.cache.Get(string(imageSet.UID))
		if ok {
			cachedImageSet := cachedImage.(mcfgv1alpha1.PinnedImageSet)
			if imageSet.Generation == cachedImageSet.Generation {
				klog.V(4).Infof("Skipping prefetch for image set %q, generation %d already complete", imageSet.Name, imageSet.Generation)
				continue
			}
		}
		if err := p.scheduleWork(ctx, p.prefetchCh, registryAuth, imageSet.Spec.PinnedImages, monitor); err != nil {
			return err
		}
	}

	if err := monitor.WaitForDone(); err != nil {
		return err
	}

	// cache the completed image sets
	for _, imageSet := range imageSets {
		imageSetCache := imageSet.DeepCopy()
		imageSetCache.Spec.PinnedImages = nil
		p.cache.Add(string(imageSet.UID), *imageSet)
	}

	return nil
}

// scheduleWork schedules the prefetch work for the images and collects the first error encountered.
func (p *PinnedImageSetManager) scheduleWork(ctx context.Context, prefetchCh chan prefetch, registryAuth *registryAuth, prefetchImages []mcfgv1alpha1.PinnedImageRef, monitor *prefetchMonitor) error {
	totalImages := len(prefetchImages)
	updateIncrement := totalImages / 4
	if updateIncrement == 0 {
		updateIncrement = 1 // Ensure there's at least one update if the image count is less than 4
	}
	scheduledImages := 0
	for _, imageRef := range prefetchImages {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			image := imageRef.Name

			// check cache if image is pulled
			// this is an optimization to speedup prefetching after requeue
			if value, found := p.cache.Get(image); found {
				imageInfo, ok := value.(imageInfo)
				if ok {
					if imageInfo.Pulled {
						scheduledImages++
						continue
					}
				}
			}

			authConfig, err := registryAuth.getAuthConfigForImage(image)
			if err != nil {
				return fmt.Errorf("failed to get auth config for image %s: %w", image, err)
			}
			monitor.Add(1)
			prefetchCh <- prefetch{
				image:   image,
				auth:    authConfig,
				monitor: monitor,
			}

			scheduledImages++
			// report status every 25% of images scheduled
			if scheduledImages%updateIncrement == 0 {
				klog.Infof("Completed scheduling %d%% of images", (scheduledImages*100)/totalImages)
			}
		}
	}
	return nil
}

func (p *PinnedImageSetManager) checkImagePayloadStorage(ctx context.Context, images []mcfgv1alpha1.PinnedImageRef, capacity int64) error {
	// calculate total required storage for all images.
	requiredStorage := int64(0)
	for _, image := range images {
		imageName := image.Name

		// check cache if image is pulled
		if value, found := p.cache.Get(imageName); found {
			imageInfo, ok := value.(imageInfo)
			if ok {
				if imageInfo.Pulled {
					continue
				}
			}
		}

		exists, err := p.criClient.ImageStatus(ctx, imageName)
		if err != nil {
			return err
		}
		if exists {
			// dont account for storage if the image already exists
			continue
		}

		// check if the image is already in the cache before fetching the size
		if value, found := p.cache.Get(imageName); found {
			imageInfo, ok := value.(imageInfo)
			if ok {
				requiredStorage += imageInfo.Size * 2
				continue
			}
			// cache is corrupted, delete the key
			klog.Warningf("corrupted cache entry for image %q, deleting", imageName)
			p.cache.Remove(imageName)
		}
		size, err := getImageSize(ctx, imageName, p.authFilePath)
		if err != nil {
			return err
		}

		// cache miss
		p.cache.Add(imageName, imageInfo{Name: imageName, Size: size})

		// account for decompression
		requiredStorage += size * 2
	}

	if requiredStorage >= capacity-p.minStorageAvailableBytes.Value() {
		klog.Errorf("%v capacity=%d, required=%d", errInsufficientStorage, capacity, requiredStorage+p.minStorageAvailableBytes.Value())
		return errInsufficientStorage
	}

	return nil
}

// ensureCrioPinnedImagesConfigFile ensures the crio config file is up to date with the pinned images.
func ensureCrioPinnedImagesConfigFile(path string, imageNames []string) error {
	cfgExists, err := hasConfigFile(path)
	if err != nil {
		return fmt.Errorf("failed to check crio config file: %w", err)
	}

	if cfgExists && len(imageNames) == 0 {
		if err := deleteCrioConfigFile(); err != nil {
			return fmt.Errorf("failed to remove CRI-O config file: %w", err)
		}
		return crioReload()
	}

	var existingCfgBytes []byte
	if cfgExists {
		existingCfgBytes, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read CRIO config file: %w", err)
		}
	}

	newCfgBytes, err := createCrioConfigFileBytes(imageNames)
	if err != nil {
		return fmt.Errorf("failed to create crio config ignition file: %w", err)
	}

	// if the existing config is the same as the new config, do nothing
	if !bytes.Equal(bytes.TrimSpace(existingCfgBytes), bytes.TrimSpace(newCfgBytes)) {
		ignFile := ctrlcommon.NewIgnFileBytes(path, newCfgBytes)
		if err := writeFiles([]ign3types.File{ignFile}, true); err != nil {
			return fmt.Errorf("failed to write CRIO config file: %w", err)
		}
		return crioReload()
	}

	return nil
}

func (p *PinnedImageSetManager) updateStatusProgressingComplete(pools []*mcfgv1.MachineConfigPool, message string) {
	node, err := p.nodeLister.Get(p.nodeName)
	if err != nil {
		klog.Errorf("Failed to get node %q: %v", p.nodeName, err)
		return
	}

	applyCfg, imageSets, err := getImageSetMetadata(p.imageSetLister, pools, nil)
	if err != nil {
		klog.Errorf("Failed to get image set apply configs: %v", err)
		return
	}

	err = upgrademonitor.UpdateMachineConfigNodeStatus(
		&upgrademonitor.Condition{
			State:   mcfgv1alpha1.MachineConfigNodePinnedImageSetsProgressing,
			Reason:  "AsExpected",
			Message: message,
		},
		nil,
		metav1.ConditionFalse,
		metav1.ConditionUnknown,
		node,
		p.mcfgClient,
		applyCfg,
		imageSets,
		p.featureGatesAccessor,
	)
	if err != nil {
		klog.Errorf("Failed to updated machine config node: %v", err)
	}

	// reset any degraded status
	err = upgrademonitor.UpdateMachineConfigNodeStatus(
		&upgrademonitor.Condition{
			State:   mcfgv1alpha1.MachineConfigNodePinnedImageSetsDegraded,
			Reason:  "AsExpected",
			Message: "All is good",
		},
		nil,
		metav1.ConditionFalse,
		metav1.ConditionUnknown,
		node,
		p.mcfgClient,
		applyCfg,
		imageSets,
		p.featureGatesAccessor,
	)
	if err != nil {
		klog.Errorf("Failed to updated machine config node: %v", err)
	}
}

func (p *PinnedImageSetManager) updateStatusError(pools []*mcfgv1.MachineConfigPool, statusErr error) {
	node, err := p.nodeLister.Get(p.nodeName)
	if err != nil {
		klog.Errorf("Failed to get node %q: %v", p.nodeName, err)
		return
	}

	applyCfg, imageSets, err := getImageSetMetadata(p.imageSetLister, pools, nil)
	if err != nil {
		klog.Errorf("Failed to get image set apply configs: %v", err)
		return
	}

	err = upgrademonitor.UpdateMachineConfigNodeStatus(
		&upgrademonitor.Condition{
			State:   mcfgv1alpha1.MachineConfigNodePinnedImageSetsProgressing,
			Reason:  "ImagePrefetch",
			Message: fmt.Sprintf("node is prefetching images: %s", node.Name),
		},
		nil,
		metav1.ConditionTrue,
		metav1.ConditionUnknown,
		node,
		p.mcfgClient,
		applyCfg,
		imageSets,
		p.featureGatesAccessor,
	)
	if err != nil {
		klog.Errorf("Failed to updated machine config node: %v", err)
	}

	applyCfg, imageSets, err = getImageSetMetadata(p.imageSetLister, pools, statusErr)
	if err != nil {
		klog.Errorf("Failed to get image set apply configs: %v", err)
		return
	}

	err = upgrademonitor.UpdateMachineConfigNodeStatus(
		&upgrademonitor.Condition{
			State:   mcfgv1alpha1.MachineConfigNodePinnedImageSetsDegraded,
			Reason:  "PrefetchFailed",
			Message: "Error: " + statusErr.Error(),
		},
		nil,
		metav1.ConditionTrue,
		metav1.ConditionUnknown,
		node,
		p.mcfgClient,
		applyCfg,
		imageSets,
		p.featureGatesAccessor,
	)
	if err != nil {
		klog.Errorf("Failed to updated machine config node: %v", err)
	}
}

// getImageSetMetadata populates observed pinned image set generation details for node level status updates to MachineConfigNode.
func getImageSetMetadata(imageSetLister mcfglistersv1alpha1.PinnedImageSetLister, pools []*mcfgv1.MachineConfigPool, statusErr error) ([]*machineconfigurationalphav1.MachineConfigNodeStatusPinnedImageSetApplyConfiguration, []mcfgv1alpha1.MachineConfigNodeSpecPinnedImageSet, error) {
	var mcnImageSets []mcfgv1alpha1.MachineConfigNodeSpecPinnedImageSet
	var imageSetApplyConfigs []*machineconfigurationalphav1.MachineConfigNodeStatusPinnedImageSetApplyConfiguration
	for _, pool := range pools {
		for _, imageSets := range pool.Spec.PinnedImageSets {
			mcnImageSets = append(mcnImageSets, mcfgv1alpha1.MachineConfigNodeSpecPinnedImageSet{
				Name: imageSets.Name,
			})
			imageSet, err := imageSetLister.Get(imageSets.Name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				klog.Errorf("Error getting pinned image set: %v", err)
				return nil, nil, err
			}

			imageSetConfig := machineconfigurationalphav1.MachineConfigNodeStatusPinnedImageSet().
				WithName(imageSet.Name).
				WithDesiredGeneration(int32(imageSet.GetGeneration()))
			if statusErr != nil {
				imageSetConfig.LastFailedGeneration = ptr.To(int32(imageSet.GetGeneration()))
				imageSetConfig.LastFailedGenerationErrors = []string{statusErr.Error()}
			} else {
				imageSetConfig.CurrentGeneration = ptr.To(int32(imageSet.GetGeneration()))
			}

			imageSetApplyConfigs = append(imageSetApplyConfigs, imageSetConfig)
		}
	}
	return imageSetApplyConfigs, mcnImageSets, nil
}

// getWorkerCount returns the number of workers to use for prefetching images.
func (p *PinnedImageSetManager) getWorkerCount() (int, error) {
	node, err := p.nodeLister.Get(p.nodeName)
	if err != nil {
		return 0, fmt.Errorf("failed to get node %q: %w", p.nodeName, err)
	}

	// default to 5 workers
	workerCount := defaultPrefetchWorkers

	// master nodes are a special case and should have a concurrency of 1 to
	// mitigate I/O contention with the control plane.
	if _, ok := node.ObjectMeta.Labels["node-role.kubernetes.io/master"]; ok {
		workerCount = defaultControlPlaneWorkers
	}

	return workerCount, nil
}

func (p *PinnedImageSetManager) resetWorkload(cancelFn context.CancelFunc) {
	p.mu.Lock()
	if p.cancelFn != nil {
		klog.V(4).Info("Reset workload")
		p.cancelFn()
	}
	p.cancelFn = cancelFn
	p.mu.Unlock()
}

func (p *PinnedImageSetManager) cancelWorkload(reason string) {
	p.mu.Lock()
	if p.cancelFn != nil {
		klog.Infof("Cancelling workload: %s", reason)
		p.cancelFn()
		p.cancelFn = nil
	}
	p.mu.Unlock()
}

// prefetchWorker is a worker that pulls images from the container runtime.
func (p *PinnedImageSetManager) prefetchWorker(ctx context.Context) {
	for task := range p.prefetchCh {
		if err := ensurePullImage(ctx, p.criClient, p.backoff, task.image, task.auth); err != nil {
			task.monitor.Error(err)
			klog.Warningf("failed to prefetch image %q: %v", task.image, err)
		}
		task.monitor.Done()

		cachedImage, ok := p.cache.Get(task.image)
		if ok {
			imageInfo, ok := cachedImage.(imageInfo)
			if !ok {
				klog.Warningf("corrupted cache entry for image %q, deleting", task.image)
				p.cache.Remove(task.image)
			}
			imageInfo.Pulled = true
			p.cache.Add(task.image, imageInfo)
		} else {
			p.cache.Add(task.image, imageInfo{Name: task.image, Pulled: true})
		}

		// throttle prefetching to avoid overloading the file system
		select {
		case <-time.After(defaultPrefetchThrottleDuration):
		case <-ctx.Done():
			return
		}
	}
}

func (p *PinnedImageSetManager) Run(workers int, stopCh <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		utilruntime.HandleCrash()
		p.queue.ShutDown()
		close(p.prefetchCh)
	}()

	if !cache.WaitForCacheSync(
		stopCh,
		p.imageSetSynced,
		p.nodeListerSynced,
		p.mcpSynced,
	) {
		klog.Errorf("failed to sync initial listers cache")
		return
	}

	workerCount, err := p.getWorkerCount()
	if err != nil {
		klog.Fatalf("failed to get worker count: %v", err)
		return
	}

	klog.Infof("Starting PinnedImageSet Manager")
	defer klog.Infof("Shutting down PinnedImageSet Manager")

	// start image prefetch workers
	for i := 0; i < workerCount; i++ {
		go p.prefetchWorker(ctx)
	}

	for i := 0; i < workers; i++ {
		go wait.Until(p.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (p *PinnedImageSetManager) addPinnedImageSet(obj interface{}) {
	imageSet := obj.(*mcfgv1alpha1.PinnedImageSet)
	if imageSet.DeletionTimestamp != nil {
		p.deletePinnedImageSet(imageSet)
		return
	}

	node, err := p.nodeLister.Get(p.nodeName)
	if err != nil {
		klog.Errorf("failed to get node %q: %v", p.nodeName, err)
		return
	}

	pools, _, err := helpers.GetPoolsForNode(p.mcpLister, node)
	if err != nil {
		klog.Errorf("error finding pools for node %s: %v", node.Name, err)
		return
	}
	if pools == nil {
		return
	}

	klog.V(4).Infof("PinnedImageSet %s added", imageSet.Name)
	for _, pool := range pools {
		if !isImageSetInPool(imageSet.Name, pool) {
			continue
		}
		p.enqueueMachineConfigPool(pool)
	}
}

func (p *PinnedImageSetManager) deletePinnedImageSet(obj interface{}) {
	imageSet, ok := obj.(*mcfgv1alpha1.PinnedImageSet)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		imageSet, ok = tombstone.Obj.(*mcfgv1alpha1.PinnedImageSet)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a PinnedImageSet %#v", obj))
			return
		}
	}

	node, err := p.nodeLister.Get(p.nodeName)
	if err != nil {
		klog.Errorf("failed to get node %q: %v", p.nodeName, err)
		return
	}

	pools, _, err := helpers.GetPoolsForNode(p.mcpLister, node)
	if err != nil {
		klog.Errorf("error finding pools for node %s: %v", node.Name, err)
		return
	}
	if pools == nil {
		return
	}

	klog.V(4).Infof("PinnedImageSet %s delete", imageSet.Name)
	for _, pool := range pools {
		if !isImageSetInPool(imageSet.Name, pool) {
			continue
		}
		p.enqueueMachineConfigPool(pool)
	}
}

func (p *PinnedImageSetManager) updatePinnedImageSet(oldObj, newObj interface{}) {
	oldImageSet := oldObj.(*mcfgv1alpha1.PinnedImageSet)
	newImageSet := newObj.(*mcfgv1alpha1.PinnedImageSet)

	imageSet, err := p.imageSetLister.Get(newImageSet.Name)
	if apierrors.IsNotFound(err) {
		return
	}

	node, err := p.nodeLister.Get(p.nodeName)
	if err != nil {
		klog.Errorf("failed to get node %q: %v", p.nodeName, err)
		return
	}

	pools, _, err := helpers.GetPoolsForNode(p.mcpLister, node)
	if err != nil {
		klog.Errorf("error finding pools for node %s: %v", node.Name, err)
		return
	}
	if pools == nil {
		return
	}

	if triggerPinnedImageSetChange(oldImageSet, newImageSet) {
		klog.V(4).Infof("PinnedImageSet %s update", imageSet.Name)
		for _, pool := range pools {
			if !isImageSetInPool(imageSet.Name, pool) {
				continue
			}
			p.cancelWorkload("PinnedImageSet update")
			p.enqueueMachineConfigPool(pool)
		}
	}
}

func (p *PinnedImageSetManager) addMachineConfigPool(obj interface{}) {
	pool := obj.(*mcfgv1.MachineConfigPool)
	if pool.DeletionTimestamp != nil {
		p.deleteMachineConfigPool(pool)
		return
	}
	selector, err := metav1.LabelSelectorAsSelector(pool.Spec.NodeSelector)
	if err != nil {
		klog.Errorf("invalid label selector: %v", err)
		return
	}

	nodes, err := p.nodeLister.List(selector)
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return
	}
	if !isNodeInPool(nodes, p.nodeName) {
		return
	}

	if pool.Spec.PinnedImageSets == nil {
		return
	}

	klog.V(4).Infof("MachineConfigPool %s added", pool.Name)
	p.enqueueMachineConfigPool(pool)
}

func (p *PinnedImageSetManager) updateMachineConfigPool(oldObj, newObj interface{}) {
	oldPool := oldObj.(*mcfgv1.MachineConfigPool)
	newPool := newObj.(*mcfgv1.MachineConfigPool)

	selector, err := metav1.LabelSelectorAsSelector(newPool.Spec.NodeSelector)
	if err != nil {
		klog.Errorf("invalid label selector: %v", err)
		return
	}

	nodes, err := p.nodeLister.List(selector)
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return
	}
	if !isNodeInPool(nodes, p.nodeName) {
		return
	}

	if triggerMachineConfigPoolChange(oldPool, newPool) {
		p.enqueueMachineConfigPool(newPool)
	}
}

func (p *PinnedImageSetManager) deleteMachineConfigPool(obj interface{}) {
	pool, ok := obj.(*mcfgv1.MachineConfigPool)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		pool, ok = tombstone.Obj.(*mcfgv1.MachineConfigPool)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a PinnedImageSet %#v", obj))
			return
		}
	}

	selector, err := metav1.LabelSelectorAsSelector(pool.Spec.NodeSelector)
	if err != nil {
		klog.Errorf("Invalid label selector: %v", err)
		return
	}

	nodes, err := p.nodeLister.List(selector)
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return
	}
	if !isNodeInPool(nodes, p.nodeName) {
		return
	}

	if err := deleteCrioConfigFile(); err != nil {
		klog.Errorf("failed to delete crio config file: %v", err)
		return
	}

	crioReload()
}

func (p *PinnedImageSetManager) enqueue(pool *mcfgv1.MachineConfigPool) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pool)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %w", pool, err))
		return
	}
	p.queue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (p *PinnedImageSetManager) worker() {
	for p.processNextItem() {
	}
}

func (p *PinnedImageSetManager) processNextItem() bool {
	key, quit := p.queue.Get()
	if quit {
		return false
	}
	defer p.queue.Done(key)

	err := p.syncHandler(key.(string))
	p.handleErr(err, key)
	return true
}

func (p *PinnedImageSetManager) handleErr(err error, key interface{}) {
	if err == nil {
		p.queue.Forget(key)
		return
	}

	if p.queue.NumRequeues(key) < maxRetriesController {
		klog.V(4).Infof("Requeue MachineConfigPool %v: %v", key, err)
		p.queue.AddRateLimited(key)
		return
	}
	utilruntime.HandleError(err)

	klog.Warningf(fmt.Sprintf(" failed: %s max retries: %d", key, maxRetriesController))
	p.queue.Forget(key)
	p.queue.AddAfter(key, 1*time.Minute)
}

// ensurePullImage first checks if the image exists locally and then will attempt to pull
// the image from the container runtime with a retry/backoff.
func ensurePullImage(ctx context.Context, client *cri.Client, backoff wait.Backoff, image string, authConfig *runtimeapi.AuthConfig) error {
	exists, err := client.ImageStatus(ctx, image)
	if err != nil {
		return err
	}
	if exists {
		klog.V(4).Infof("image %q already exists", image)
		return nil
	}

	var lastErr error
	tries := 0
	err = wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		tries++
		err := client.PullImage(ctx, image, authConfig)
		if err != nil {
			lastErr = err
			// fail fast if out of space
			if isErrNoSpace(err) {
				return false, err
			}
			return false, nil
		}
		return true, nil
	})
	// this is only an error if ctx has error or backoff limits are exceeded
	if err != nil {
		return fmt.Errorf("%w %q (%d tries): %w: %w", errFailedToPullImage, image, tries, err, lastErr)
	}

	// successful pull
	klog.V(4).Infof("image %q pulled", image)
	return nil
}

func isErrNoSpace(err error) bool {
	if err == syscall.ENOSPC {
		return true
	}
	if werr, ok := err.(*os.PathError); ok {
		if werr.Err == syscall.ENOSPC {
			return true
		}
	}
	return false
}

func uniqueSortedImageNames(images []mcfgv1alpha1.PinnedImageRef) []string {
	seen := make(map[string]struct{})
	var unique []string

	for _, image := range images {
		if _, ok := seen[image.Name]; !ok {
			seen[image.Name] = struct{}{}
			unique = append(unique, image.Name)
		}
	}

	sort.Strings(unique)

	return unique
}

func crioReload() error {
	serviceName := constants.CRIOServiceName
	if err := reloadService(serviceName); err != nil {
		return fmt.Errorf("could not apply update: reloading %s configuration failed. Error: %w", serviceName, err)
	}
	return nil
}

func deleteCrioConfigFile() error {
	// remove the crio config file
	if err := os.Remove(crioPinnedImagesDropInFilePath); err != nil {
		return fmt.Errorf("failed to remove crio config file: %w", err)
	}
	klog.Infof("removed crio config file: %s", crioPinnedImagesDropInFilePath)

	return nil
}

func hasConfigFile(path string) (bool, error) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check crio config file: %w", err)
	}

	return true, nil
}

// createCrioConfigFileBytes creates a crio config file with the pinned images.
func createCrioConfigFileBytes(images []string) ([]byte, error) {
	type tomlConfig struct {
		Crio struct {
			Image struct {
				PinnedImages []string `toml:"pinned_images,omitempty"`
			} `toml:"image"`
		} `toml:"crio"`
	}

	tomlConf := tomlConfig{}
	tomlConf.Crio.Image.PinnedImages = images

	var buf bytes.Buffer
	encoder := toml.NewEncoder(&buf)
	if err := encoder.Encode(tomlConf); err != nil {
		return nil, fmt.Errorf("failed to encode toml: %w", err)
	}

	return buf.Bytes(), nil
}

func isImageSetInPool(imageSet string, pool *mcfgv1.MachineConfigPool) bool {
	for _, set := range pool.Spec.PinnedImageSets {
		if set.Name == imageSet {
			return true
		}
	}
	return false
}

func isNodeInPool(nodes []*corev1.Node, nodeName string) bool {
	for _, n := range nodes {
		if n.Name == nodeName {
			return true
		}
	}
	return false
}

// imageCache is a thread-safe cache for storing image information.
type imageCache struct {
	mu    sync.RWMutex
	cache *lru.Cache
}

func newImageCache(maxEntries int) *imageCache {
	return &imageCache{
		cache: lru.New(maxEntries),
	}
}

func (c *imageCache) Add(key, value interface{}) {
	c.mu.Lock()
	c.cache.Add(key, value)
	c.mu.Unlock()
}

func (c *imageCache) Get(key interface{}) (value interface{}, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cache.Get(key)
}

func (c *imageCache) Clear() {
	c.mu.Lock()
	c.cache.Clear()
	c.mu.Unlock()
}

func (c *imageCache) Remove(key interface{}) {
	c.mu.Lock()
	c.cache.Remove(key)
	c.mu.Unlock()
}

// imageInfo is used to store image information in the cache.
type imageInfo struct {
	Name   string
	Size   int64
	Pulled bool
}

func getImageSize(ctx context.Context, imageName, authFilePath string) (int64, error) {
	args := []string{
		"manifest",
		"--log-level", "error", // suppress warn log output
		"inspect",
		"--authfile", authFilePath,
		imageName,
	}

	output, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput()
	if err != nil && strings.Contains(err.Error(), "manifest unknown") {
		return 0, errNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("failed to execute podman manifest inspect for %q: %w", imageName, err)
	}

	var manifest ocispec.Manifest
	err = json.Unmarshal(output, &manifest)
	if err != nil {
		return 0, fmt.Errorf("failed to unmarshal manifest for %q: %w", imageName, err)
	}

	var totalSize int64
	for _, layer := range manifest.Layers {
		totalSize += layer.Size
	}

	return totalSize, nil
}

func triggerPinnedImageSetChange(old, new *mcfgv1alpha1.PinnedImageSet) bool {
	if old.DeletionTimestamp != new.DeletionTimestamp {
		return true
	}
	if !reflect.DeepEqual(old.Spec, new.Spec) {
		return true
	}
	return false
}

func triggerMachineConfigPoolChange(old, new *mcfgv1.MachineConfigPool) bool {
	if old.DeletionTimestamp != new.DeletionTimestamp {
		return true
	}
	if !reflect.DeepEqual(old.Spec.PinnedImageSets, new.Spec.PinnedImageSets) {
		return true
	}
	return false
}

// registryAuth manages the registry authentication for pulling images.
type registryAuth struct {
	mu   sync.RWMutex
	auth map[string]*runtimeapi.AuthConfig
	reg  map[string]*sysregistriesv2.Registry
}

func newRegistryAuth(authFilePath, registryCfgPath string) (*registryAuth, error) {
	data, err := os.ReadFile(authFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read authfile: %w", err)
	}

	type authEntry struct {
		Auth  string `json:"auth"`
		Email string `json:"email"`
	}

	type auths struct {
		Auths map[string]authEntry `json:"auths"`
	}

	var a auths
	err = json.Unmarshal(data, &a)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal pull secret: %w", err)
	}

	authMap := make(map[string]*runtimeapi.AuthConfig, len(a.Auths))
	for registry, entry := range a.Auths {
		// auth is base64 encoded and in the format username:password
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			return nil, fmt.Errorf("failed to decode auth for registry %q: %w", registry, err)
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid auth for registry %q", registry)
		}
		authMap[registry] = &runtimeapi.AuthConfig{
			Username: parts[0],
			Password: parts[1],
		}
	}

	sys := &types.SystemContext{
		SystemRegistriesConfPath: registryCfgPath,
	}
	regs, err := sysregistriesv2.GetRegistries(sys)
	if err != nil {
		return nil, fmt.Errorf("failed to get registries: %w", err)
	}

	regMap := make(map[string]*sysregistriesv2.Registry, len(regs))
	for _, reg := range regs {
		regMap[reg.Prefix] = &reg
	}

	return &registryAuth{auth: authMap, reg: regMap}, nil
}

func (r *registryAuth) getAuth(domain string) *runtimeapi.AuthConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.auth[domain]
}

func (r *registryAuth) getRegistry(prefix string) *sysregistriesv2.Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reg[prefix]
}

func (r *registryAuth) getAuthConfigForImage(image string) (*runtimeapi.AuthConfig, error) {
	parsed, err := reference.ParseNormalizedNamed(strings.TrimSpace(image))
	if err != nil {
		return nil, err
	}

	// check if the image is has a mirror if found match source to existing auth if possible
	parts := strings.Split(image, "@")
	registry := r.getRegistry(parts[0])
	if registry != nil {
		sources, err := registry.PullSourcesFromReference(parsed)
		if err != nil {
			return nil, err
		}
		for _, source := range sources {
			pref, err := reference.ParseNamed(source.Endpoint.Location)
			if err != nil {
				return nil, err
			}
			if auth := r.getAuth(reference.Domain(pref)); auth != nil {
				return auth, nil
			}
		}
	}

	if auth := r.getAuth(reference.Domain(parsed)); auth != nil {
		return auth, nil

	}

	// no auth found for the image this is not an error for public images
	klog.V(4).Infof("no auth found for image: %s", image)
	return nil, nil
}

// prefetch represents a task to prefetch an image.
type prefetch struct {
	image   string
	auth    *runtimeapi.AuthConfig
	monitor *prefetchMonitor
}

// prefetchMonitor is used to monitor the status of prefetch operations.
type prefetchMonitor struct {
	once  sync.Once
	errFn func(error)
	err   error
	wg    sync.WaitGroup
}

func newPrefetchMonitor() *prefetchMonitor {
	m := &prefetchMonitor{}
	m.errFn = func(err error) {
		m.once.Do(func() {
			m.err = err
		})
	}
	return m
}

// Add increments the number of prefetch operations the monitor is waiting for.
func (m *prefetchMonitor) Add(i int) {
	m.wg.Add(i)
}

func (m *prefetchMonitor) finalize(err error) {
	m.once.Do(func() {
		m.err = err
	})
}

// Error is called when an error occurs during prefetching.
func (m *prefetchMonitor) Error(err error) {
	m.finalize(err)
}

// Done decrements the number of prefetch operations the monitor is waiting for.
func (m *prefetchMonitor) Done() {
	m.wg.Done()
}

// WaitForDone waits for the prefetch operations to complete and returns the first error encountered.
func (m *prefetchMonitor) WaitForDone() error {
	m.wg.Wait()
	return m.err
}
