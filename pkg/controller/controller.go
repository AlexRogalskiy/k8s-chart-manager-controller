package controller

import (
	"context"
	"errors"
	"time"

	crv1alpha1 "github.com/logicmonitor/k8s-chart-manager-controller/pkg/apis/v1alpha1"
	chartmgrclient "github.com/logicmonitor/k8s-chart-manager-controller/pkg/client"
	"github.com/logicmonitor/k8s-chart-manager-controller/pkg/config"
	lmhelm "github.com/logicmonitor/k8s-chart-manager-controller/pkg/lmhelm"
	log "github.com/sirupsen/logrus"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// Controller is the Kubernetes controller object for LogicMonitor
// chartmgrs.
type Controller struct {
	*chartmgrclient.Client
	ChartMgrScheme *runtime.Scheme
	Config         *config.Config
	HelmClient     *lmhelm.Client
}

// New instantiates and returns a Controller and an error if any.
func New(chartmgrconfig *config.Config) (*Controller, error) {
	// Instantiate the Kubernetes in cluster config.
	restconfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	// Instantiate the ChartMgr client.
	client, chartmgrscheme, err := chartmgrclient.NewForConfig(restconfig)
	if err != nil {
		return nil, err
	}

	// initialize our LM helm wrapper struct
	helmClient := &lmhelm.Client{}
	err = helmClient.Init(chartmgrconfig, restconfig)
	if err != nil {
		return nil, err
	}

	// start a controller on instances of our custom resource
	c := &Controller{
		Client:         client,
		ChartMgrScheme: chartmgrscheme,
		Config:         chartmgrconfig,
		HelmClient:     helmClient,
	}
	return c, nil
}

// Run starts a Chart Manager resource controller.
func (c *Controller) Run(ctx context.Context) error {
	// Manage Chart Manager objects
	err := c.manage(ctx)
	if err != nil {
		return err
	}

	log.Info("Successfully started Chart Manager controller")
	<-ctx.Done()

	return ctx.Err()
}

func (c *Controller) manage(ctx context.Context) error {
	_, controller := cache.NewInformer(
		cache.NewListWatchFromClient(
			c.RESTClient,
			crv1alpha1.ChartMgrResourcePlural,
			apiv1.NamespaceAll,
			fields.Everything(),
		),
		&crv1alpha1.ChartManager{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.addFunc,
			UpdateFunc: c.updateFunc,
			DeleteFunc: c.deleteFunc,
		},
	)

	go controller.Run(ctx.Done())
	return nil
}

func (c *Controller) addFunc(obj interface{}) {
	go func(obj interface{}) {
		chartmgr := obj.(*crv1alpha1.ChartManager)
		rls, err := CreateOrUpdateChartMgr(chartmgr, c.HelmClient)
		if err != nil {
			log.Errorf("%s", err)
			c.updateChartMgrStatus(chartmgr, rls, err.Error())
			return
		}

		err = c.updateStatus(chartmgr, rls)
		if err != nil {
			return
		}
		log.Infof("Chart Manager %s status is %s", chartmgr.Name, rls.Status())
		log.Infof("Created Chart Manager: %s", chartmgr.Name)
	}(obj)
}

func (c *Controller) updateFunc(oldObj, newObj interface{}) {
	go func(oldObj interface{}, newObj interface{}) {
		_ = oldObj.(*crv1alpha1.ChartManager)
		newChartMgr := newObj.(*crv1alpha1.ChartManager)

		rls, err := CreateOrUpdateChartMgr(newChartMgr, c.HelmClient)
		if err != nil {
			log.Errorf("%s", err)
			c.updateChartMgrStatus(newChartMgr, rls, err.Error())
			return
		}

		if lmhelm.CreateOnly(newChartMgr) {
			log.Infof("CreateOnly mode. Ignoring update of chart manager %s.", newChartMgr.Name)
			return
		}

		err = c.updateStatus(newChartMgr, rls)
		if err != nil {
			return
		}
		log.Infof("Updated Chart Manager: %s", newChartMgr.Name)
	}(oldObj, newObj)
}

func (c *Controller) deleteFunc(obj interface{}) {
	go func(obj interface{}) {
		chartmgr := obj.(*crv1alpha1.ChartManager)

		_, err := DeleteChartMgr(chartmgr, c.HelmClient)
		if err != nil {
			log.Errorf("Failed to delete Chart Manager: %v", err)
			return
		}
		log.Infof("Deleted Chart Manager: %s", chartmgr.Name)
	}(obj)
}

func (c *Controller) updateStatus(chartmgr *crv1alpha1.ChartManager, rls *lmhelm.Release) error {
	err := c.waitForReleaseToDeploy(rls)
	if err != nil {
		log.Errorf("Failed to verify that release %v deployed: %v", rls.Name(), err)
		c.updateChartMgrStatus(chartmgr, rls, err.Error())
	} else {
		log.Infof("Chart Manager %s release %s status is Deployed", chartmgr.Name, rls.Name())
		c.updateChartMgrStatus(chartmgr, rls, string(rls.Status()))
	}
	return err
}

func (c *Controller) updateChartMgrStatus(chartmgr *crv1alpha1.ChartManager, rls *lmhelm.Release, message string) {
	log.Debugf("Updating Chart Manager status: state=%s release=%s", rls.Status(), rls.Name())
	chartmgrCopy := chartmgr.DeepCopy()
	chartmgrCopy.Status = crv1alpha1.ChartMgrStatus{
		State:       rls.Status(),
		ReleaseName: rls.Name(),
		Message:     message,
	}

	err := c.put(chartmgrCopy)
	if err != nil {
		log.Errorf("Failed to update status: %v", err)
	}
}

func (c *Controller) put(chartmgr *crv1alpha1.ChartManager) error {
	return c.RESTClient.Put().
		Name(chartmgr.ObjectMeta.Name).
		Namespace(chartmgr.ObjectMeta.Namespace).
		Resource(crv1alpha1.ChartMgrResourcePlural).
		Body(chartmgr).
		Do().
		Error()
}

func (c *Controller) waitForReleaseToDeploy(rls *lmhelm.Release) error {
	timeout := time.After(2 * time.Minute)
	ticker := time.NewTicker(30 * time.Second)

	for c := ticker.C; ; <-c {
		select {
		case <-timeout:
			return errors.New("Timed out waiting for release to deploy")
		default:
			log.Debugf("Checking status of release %s", rls.Name())
			if rls.Deployed() {
				return nil
			}
		}
	}
}
