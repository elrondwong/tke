/*
 * Tencent is pleased to support the open source community by making TKEStack
 * available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use
 * this file except in compliance with the License. You may obtain a copy of the
 * License at
 *
 * https://opensource.org/licenses/Apache-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OF ANY KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations under the License.
 */

package installer

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"time"

	applicationversiondclient "tkestack.io/tke/api/client/clientset/versioned/typed/application/v1"

	"github.com/pkg/errors"
	"github.com/thoas/go-funk"
	"gopkg.in/yaml.v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	platformv1 "tkestack.io/tke/api/client/clientset/versioned/typed/platform/v1"
	registryversionedclient "tkestack.io/tke/api/client/clientset/versioned/typed/registry/v1"
	"tkestack.io/tke/cmd/tke-installer/app/installer/constants"
	"tkestack.io/tke/cmd/tke-installer/app/installer/images"
	"tkestack.io/tke/cmd/tke-installer/app/installer/types"
	cronhpaimage "tkestack.io/tke/pkg/platform/controller/addon/cronhpa/images"
	tappimage "tkestack.io/tke/pkg/platform/controller/addon/tappcontroller/images"
	clusterprovider "tkestack.io/tke/pkg/platform/provider/cluster"
	"tkestack.io/tke/pkg/platform/util"
	configv1 "tkestack.io/tke/pkg/registry/apis/config/v1"
	"tkestack.io/tke/pkg/spec"
	"tkestack.io/tke/pkg/util/apiclient"
	"tkestack.io/tke/pkg/util/containerregistry"
	"tkestack.io/tke/pkg/util/file"
	"tkestack.io/tke/pkg/util/version"

	// import platform schema
	_ "tkestack.io/tke/api/platform/install"
)

const (
	registryCmName = "tke-registry-api"
	registryCmKey  = "tke-registry-config.yaml"
)

var upgradeProviderResImage string

func (t *TKE) upgradeSteps() {
	containerregistry.Init(t.Para.Config.Registry.Domain(), t.Para.Config.Registry.Namespace())
	tkeVersion, k8sValidVersions, err := util.GetPlatformVersionsFromClusterInfo(context.Background(), t.globalClient)
	if err != nil {
		log.Fatalf("get platform version from cluster info failed: %v", err)
	}
	result := version.Compare(tkeVersion, spec.TKEVersion)

	switch {
	// current platform version is higher than installer version
	case result > 0:
		log.Fatalf("can't upgrade, platform's version %s is higher than installer's version %s", tkeVersion, spec.TKEVersion)
	// current platform version is euaql to installer version
	case result == 0:
		if len(k8sValidVersions) == len(spec.K8sVersions) {
			log.Fatalf("can't upgrade, platform's version %s is equal to installer's version %s, please prepare your custom upgrade images before upgrade", tkeVersion, spec.TKEVersion)
		}
		upgradeProviderResImage = containerregistry.GetImagePrefix(images.Get().ProviderRes.Name + ":" + k8sValidVersions[len(k8sValidVersions)-1])
		t.steps = []types.Handler{
			{
				Name: "Login registry",
				Func: t.loginRegistry,
			},
			{
				Name: "Tag images",
				Func: t.tagImages,
			},
			{
				Name: "Push images",
				Func: t.pushImages,
			},
			{
				Name: "Upgrade tke-platform-controller",
				Func: t.upgradeTKEPlatformController,
			},
		}
	// installer version is higher than current platform version
	case result < 0:
		upgradeProviderResImage = images.Get().ProviderRes.FullName()
		if !t.Para.Config.Registry.IsOfficial() {
			t.steps = append(t.steps, []types.Handler{
				{
					Name: "Login registry",
					Func: t.loginRegistry,
				},
				{
					Name: "Load images",
					Func: t.loadImages,
				},
				{
					Name: "Tag images",
					Func: t.tagImages,
				},
				{
					Name: "Push images",
					Func: t.pushImages,
				},
			}...)
		}
		t.steps = append(t.steps, []types.Handler{
			{
				Name: "Prepare images before upgrade",
				Func: t.prepareImages,
			},
			{
				Name: "Upgrade tke-platform-api",
				Func: t.upgradeTKEPlatformAPI,
			},
			{
				Name: "Upgrade tke-platform-controller",
				Func: t.upgradeTKEPlatformController,
			},
			{
				Name: "Upgrade tke-monitor-api",
				Func: t.upgradeTKEMonitorAPI,
			},
			{
				Name: "Upgrade tke-monitor-controller",
				Func: t.upgradeTKEMonitorController,
			},
			{
				Name: "Upgrade tke-application-api",
				Func: t.upgradeTKEApplicationAPI,
			},
			{
				Name: "Upgrade tke-application-controller",
				Func: t.upgradeTKEApplicationController,
			},
			{
				Name: "Upgrade tke-logagent-api",
				Func: t.upgradeTKELogagentAPI,
			},
			{
				Name: "Upgrade tke-logagent-controller",
				Func: t.upgradeTKELogagentController,
			},
			// upgrade gateway should always be the last com to upgrade
			{
				Name: "Upgrade tke-gateway",
				Func: t.upgradeTKEGateway,
			},
		}...)

		t.steps = append(t.steps, []types.Handler{
			{
				Name: "Patch platform versions in cluster info",
				Func: t.patchPlatformVersion,
			},
		}...)

		t.steps = append(t.steps, []types.Handler{
			{
				Name: "Upgrade TAPP",
				Func: t.upgradeTAPP,
			},
			{
				Name: "Upgrade CronHPA",
				Func: t.upgradeCronHPA,
			},
		}...)

		if t.Para.Config.Registry.ThirdPartyRegistry == nil &&
			t.Para.Config.Registry.TKERegistry != nil {
			t.steps = append(t.steps, []types.Handler{
				{
					Name: "Import charts",
					Func: t.importCharts,
				},
			}...)
		}
	}

	t.steps = funk.Filter(t.steps, func(step types.Handler) bool {
		return !funk.ContainsString(t.Para.Config.SkipSteps, step.Name)
	}).([]types.Handler)

	t.log.Info("Steps:")
	for i, step := range t.steps {
		t.log.Infof("%d %s", i, step.Name)
	}
}

func (t *TKE) upgradeTKEPlatformAPI(ctx context.Context) error {
	return t.upgradeDeplImage(ctx, images.Get().TKEPlatformAPI)
}

func (t *TKE) upgradeTKEPlatformController(ctx context.Context) error {
	com := "tke-platform-controller"
	depl, err := t.globalClient.AppsV1().Deployments(t.namespace).Get(ctx, com, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if len(depl.Spec.Template.Spec.Containers) == 0 {
		return fmt.Errorf("%s has no containers", com)
	}
	depl.Spec.Template.Spec.Containers[0].Image = images.Get().TKEPlatformController.FullName()

	if len(depl.Spec.Template.Spec.InitContainers) == 0 {
		return fmt.Errorf("%s has no initContainers", com)
	}

	depl.Spec.Template.Spec.InitContainers[0].Image = upgradeProviderResImage

	_, err = t.globalClient.AppsV1().Deployments(t.namespace).Update(ctx, depl, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	return wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
		ok, err := apiclient.CheckDeployment(ctx, t.globalClient, t.namespace, com)
		if err != nil {
			return false, nil
		}
		return ok, nil
	})
}

func (t *TKE) upgradeTKEMonitorAPI(ctx context.Context) error {
	return t.upgradeDeplImage(ctx, images.Get().TKEMonitorAPI)
}

func (t *TKE) upgradeTKEMonitorController(ctx context.Context) error {
	return t.upgradeDeplImage(ctx, images.Get().TKEMonitorController)
}

func (t *TKE) upgradeTKEApplicationAPI(ctx context.Context) error {
	return t.upgradeDeplImage(ctx, images.Get().TKEApplicationAPI)
}

func (t *TKE) upgradeTKEApplicationController(ctx context.Context) error {
	return t.upgradeDeplImage(ctx, images.Get().TKEApplicationController)
}

func (t *TKE) upgradeTKELogagentAPI(ctx context.Context) error {
	return t.upgradeDeplImage(ctx, images.Get().TKELogagentAPI)
}

func (t *TKE) upgradeTKELogagentController(ctx context.Context) error {
	return t.upgradeDeplImage(ctx, images.Get().TKELogagentController)
}

func (t *TKE) upgradeDeplImage(ctx context.Context, image containerregistry.Image) error {
	com := image.Name
	depl, err := t.globalClient.AppsV1().Deployments(t.namespace).Get(ctx, com, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if len(depl.Spec.Template.Spec.Containers) == 0 {
		return fmt.Errorf("%s has no containers", com)
	}
	depl.Spec.Template.Spec.Containers[0].Image = image.FullName()

	_, err = t.globalClient.AppsV1().Deployments(t.namespace).Update(ctx, depl, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	return wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
		ok, err := apiclient.CheckDeployment(ctx, t.globalClient, t.namespace, com)
		if err != nil {
			return false, nil
		}
		return ok, nil
	})
}

func (t *TKE) upgradeTKEGateway(ctx context.Context) error {
	com := "tke-gateway"
	ds, err := t.globalClient.AppsV1().DaemonSets(t.namespace).Get(ctx, com, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if len(ds.Spec.Template.Spec.Containers) == 0 {
		return fmt.Errorf("%s has no containers", com)
	}
	ds.Spec.Template.Spec.Containers[0].Image = images.Get().TKEGateway.FullName()

	_, err = t.globalClient.AppsV1().DaemonSets(t.namespace).Update(ctx, ds, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	return wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
		ok, err := apiclient.CheckDaemonset(ctx, t.globalClient, t.namespace, com)
		if err != nil {
			return false, nil
		}
		return ok, nil
	})
}

func (t *TKE) prepareForUpgrade(ctx context.Context) error {
	t.namespace = namespace

	_ = t.loadTKEData()

	if !file.Exists(t.Config.Kubeconfig) || !file.IsFile(t.Config.Kubeconfig) {
		return fmt.Errorf("kubeconfig %s doesn't exist", t.Config.Kubeconfig)
	}
	config, err := clientcmd.BuildConfigFromFlags("", t.Config.Kubeconfig)
	if err != nil {
		return err
	}
	t.globalClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	t.platformClient, err = platformv1.NewForConfig(config)
	if err != nil {
		return err
	}
	t.registryClient, err = registryversionedclient.NewForConfig(config)
	if err != nil {
		return err
	}
	t.applicationClient, err = applicationversiondclient.NewForConfig(config)
	if err != nil {
		return err
	}
	t.Cluster, err = clusterprovider.GetV1ClusterByName(ctx, t.platformClient, "global", clusterprovider.AdminUsername)
	if err != nil {
		return err
	}
	t.Para.Cluster = *t.Cluster.Cluster
	t.Para.Config.Registry.UserInputRegistry.Domain = t.Config.RegistryDomain
	t.Para.Config.Registry.UserInputRegistry.Username = t.Config.RegistryUsername
	t.Para.Config.Registry.UserInputRegistry.Password = []byte(t.Config.RegistryPassword)
	t.Para.Config.Registry.UserInputRegistry.Namespace = t.Config.RegistryNamespace
	err = t.loadRegistry(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			t.log.Infof("Not found %s", registryCmName)
			if t.Para.Config.Registry.ThirdPartyRegistry == nil {
				t.log.Infof("Not found third party registry")
				t.Para.Config.Registry.ThirdPartyRegistry = &types.ThirdPartyRegistry{}
			}
		} else {
			return err
		}
	}
	return nil
}

func (t *TKE) loadRegistry(ctx context.Context) error {
	registryCm, err := t.globalClient.CoreV1().ConfigMaps(t.namespace).Get(ctx, registryCmName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	registryConfig := &configv1.RegistryConfiguration{}
	err = yaml.Unmarshal([]byte(registryCm.Data[registryCmKey]), registryConfig)
	if err != nil {
		return err
	}
	t.Para.Config.Registry.TKERegistry = &types.TKERegistry{
		Domain:        registryConfig.DomainSuffix,
		HarborEnabled: registryConfig.HarborEnabled,
		HarborCAFile:  registryConfig.HarborCAFile,
		Namespace:     "library",
		Username:      registryConfig.Security.AdminUsername,
		Password:      []byte(registryConfig.Security.AdminPassword),
	}
	return nil
}

func (t *TKE) loginRegistry(ctx context.Context) error {
	containerregistry.Init(t.Para.Config.Registry.Domain(), t.Para.Config.Registry.Namespace())
	dir := path.Join(constants.DockerCertsDir, t.Para.Config.Registry.Domain())
	_ = os.MkdirAll(dir, 0777)
	caCert, _ := ioutil.ReadFile(constants.CACrtFile)
	err := ioutil.WriteFile(path.Join(dir, "ca.crt"), caCert, 0644)
	if err != nil {
		return err
	}
	cmd := exec.Command("docker", "login",
		"--username", t.Para.Config.Registry.Username(),
		"--password", string(t.Para.Config.Registry.Password()),
		t.Para.Config.Registry.Domain(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return errors.New(string(out))
		}
		return err
	}
	return nil
}

func (t *TKE) upgradeTAPP(ctx context.Context) error {
	t.log.Infof("start to upgrade TAPPControllers, TAPPControllers latest version is %s", tappimage.LatestVersion)
	tapps, err := t.platformClient.TappControllers().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, tapp := range tapps.Items {
		t.log.Infof("upgrade %s from %s to %s", tapp.Name, tapp.Spec.Version, tappimage.LatestVersion)
		tapp.Spec.Version = tappimage.LatestVersion
		_, err = t.platformClient.TappControllers().Update(ctx, &tapp, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	t.log.Infof("end TAPPControllers upgrade process")

	return nil
}

func (t *TKE) upgradeCronHPA(ctx context.Context) error {
	t.log.Infof("start to upgrade CronHPAs, CronHPAs latest version is %s", cronhpaimage.LatestVersion)
	cronhpas, err := t.platformClient.CronHPAs().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, cronhpa := range cronhpas.Items {
		t.log.Infof("upgrade %s from %s to %s", cronhpa.Name, cronhpa.Spec.Version, cronhpaimage.LatestVersion)
		cronhpa.Spec.Version = cronhpaimage.LatestVersion
		_, err = t.platformClient.CronHPAs().Update(ctx, &cronhpa, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	t.log.Infof("end CronHPAs upgrade process")

	return nil
}
