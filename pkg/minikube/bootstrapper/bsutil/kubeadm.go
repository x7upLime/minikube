/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package bsutil will eventually be renamed to kubeadm package after getting rid of older one
package bsutil

import (
	"bytes"
	"fmt"
	"path"

	"github.com/blang/semver/v4"
	"github.com/pkg/errors"
	"k8s.io/klog/v2"
	"k8s.io/minikube/pkg/minikube/bootstrapper/bsutil/ktmpl"
	"k8s.io/minikube/pkg/minikube/cni"
	"k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/constants"
	"k8s.io/minikube/pkg/minikube/cruntime"
	"k8s.io/minikube/pkg/minikube/vmpath"
	"k8s.io/minikube/pkg/util"
)

// Container runtimes
const remoteContainerRuntime = "remote"

// GenerateKubeadmYAML generates the kubeadm.yaml file
func GenerateKubeadmYAML(cc config.ClusterConfig, n config.Node, r cruntime.Manager) ([]byte, error) {
	k8s := cc.KubernetesConfig
	version, err := util.ParseKubernetesVersion(k8s.KubernetesVersion)
	if err != nil {
		return nil, errors.Wrap(err, "parsing Kubernetes version")
	}

	// parses a map of the feature gates for kubeadm and component
	kubeadmFeatureArgs, componentFeatureArgs, err := parseFeatureArgs(k8s.FeatureGates)
	if err != nil {
		return nil, errors.Wrap(err, "parses feature gate config for kubeadm and component")
	}

	// In case of no port assigned, use default
	cp, err := config.PrimaryControlPlane(&cc)
	if err != nil {
		return nil, errors.Wrap(err, "getting control plane")
	}
	nodePort := cp.Port
	if nodePort <= 0 {
		nodePort = constants.APIServerPort
	}

	cgroupDriver, err := r.CGroupDriver()
	if err != nil {
		if !r.Active() {
			return nil, cruntime.ErrContainerRuntimeNotRunning
		}
		return nil, errors.Wrap(err, "getting cgroup driver")
	}
	// TODO: investigate why containerd (v1.6.15) does not work with k8s (v1.25.3) when both are set to use systemd cgroup driver
	// issue: https://github.com/kubernetes/minikube/issues/15633
	// until this is fixed, the workaround is to configure kubelet to use cgroupfs when containerd is using systemd
	// note: pkg/minikube/bootstrapper/bsutil/kubeadm_test.go::TestGenerateKubeadmYAML also extects this override (for now)
	if cc.KubernetesConfig.ContainerRuntime == constants.Containerd && cgroupDriver == constants.SystemdCgroupDriver {
		cgroupDriver = constants.CgroupfsCgroupDriver
	}

	componentOpts, err := createExtraComponentConfig(k8s.ExtraOptions, version, componentFeatureArgs, cp)
	if err != nil {
		return nil, errors.Wrap(err, "generating extra component config for kubeadm")
	}

	cnm, err := cni.New(&cc)
	if err != nil {
		return nil, errors.Wrap(err, "cni")
	}

	podCIDR := cnm.CIDR()
	overrideCIDR := k8s.ExtraOptions.Get("pod-network-cidr", Kubeadm)
	if overrideCIDR != "" {
		podCIDR = overrideCIDR
	}
	klog.Infof("Using pod CIDR: %s", podCIDR)

	// ref: https://kubernetes.io/docs/reference/config-api/kubelet-config.v1beta1/#kubelet-config-k8s-io-v1beta1-KubeletConfiguration
	kubeletConfigOpts := kubeletConfigOpts(k8s.ExtraOptions)
	// set hairpin mode to hairpin-veth to achieve hairpin NAT, because promiscuous-bridge assumes the existence of a container bridge named cbr0
	// ref: https://kubernetes.io/docs/tasks/debug/debug-application/debug-service/#a-pod-fails-to-reach-itself-via-the-service-ip
	kubeletConfigOpts["hairpinMode"] = k8s.ExtraOptions.Get("hairpin-mode", Kubelet)
	if kubeletConfigOpts["hairpinMode"] == "" {
		kubeletConfigOpts["hairpinMode"] = "hairpin-veth"
	}
	// set timeout for all runtime requests except long running requests - pull, logs, exec and attach
	kubeletConfigOpts["runtimeRequestTimeout"] = k8s.ExtraOptions.Get("runtime-request-timeout", Kubelet)
	if kubeletConfigOpts["runtimeRequestTimeout"] == "" {
		kubeletConfigOpts["runtimeRequestTimeout"] = "15m"
	}

	opts := struct {
		CertDir                    string
		ServiceCIDR                string
		PodSubnet                  string
		AdvertiseAddress           string
		APIServerPort              int
		KubernetesVersion          string
		EtcdDataDir                string
		EtcdExtraArgs              map[string]string
		ClusterName                string
		NodeName                   string
		DNSDomain                  string
		CRISocket                  string
		ImageRepository            string
		ComponentOptions           []componentOptions
		FeatureArgs                map[string]bool
		NodeIP                     string
		CgroupDriver               string
		ClientCAFile               string
		StaticPodPath              string
		ControlPlaneAddress        string
		KubeProxyOptions           map[string]string
		ResolvConfSearchRegression bool
		KubeletConfigOpts          map[string]string
	}{
		CertDir:           vmpath.GuestKubernetesCertsDir,
		ServiceCIDR:       constants.DefaultServiceCIDR,
		PodSubnet:         podCIDR,
		AdvertiseAddress:  n.IP,
		APIServerPort:     nodePort,
		KubernetesVersion: k8s.KubernetesVersion,
		EtcdDataDir:       EtcdDataDir(),
		EtcdExtraArgs:     etcdExtraArgs(k8s.ExtraOptions),
		ClusterName:       cc.Name,
		// kubeadm uses NodeName as the --hostname-override parameter, so this needs to be the name of the machine
		NodeName:                   KubeNodeName(cc, n),
		CRISocket:                  r.SocketPath(),
		ImageRepository:            k8s.ImageRepository,
		ComponentOptions:           componentOpts,
		FeatureArgs:                kubeadmFeatureArgs,
		DNSDomain:                  k8s.DNSDomain,
		NodeIP:                     n.IP,
		CgroupDriver:               cgroupDriver,
		ClientCAFile:               path.Join(vmpath.GuestKubernetesCertsDir, "ca.crt"),
		StaticPodPath:              vmpath.GuestManifestsDir,
		ControlPlaneAddress:        constants.ControlPlaneAlias,
		KubeProxyOptions:           createKubeProxyOptions(k8s.ExtraOptions),
		ResolvConfSearchRegression: HasResolvConfSearchRegression(k8s.KubernetesVersion),
		KubeletConfigOpts:          kubeletConfigOpts,
	}

	if k8s.ServiceCIDR != "" {
		opts.ServiceCIDR = k8s.ServiceCIDR
	}

	configTmpl := ktmpl.V1Alpha3
	// v1beta1 works in v1.13, but isn't required until v1.14.
	if version.GTE(semver.MustParse("1.14.0-alpha.0")) {
		configTmpl = ktmpl.V1Beta1
	}
	// v1beta2 isn't required until v1.17.
	if version.GTE(semver.MustParse("1.17.0")) {
		configTmpl = ktmpl.V1Beta2
	}

	// v1beta3 isn't required until v1.23.
	if version.GTE(semver.MustParse("1.23.0")) {
		configTmpl = ktmpl.V1Beta3
	}
	klog.Infof("kubeadm options: %+v", opts)
	b := bytes.Buffer{}
	if err := configTmpl.Execute(&b, opts); err != nil {
		return nil, err
	}
	klog.Infof("kubeadm config:\n%s\n", b.String())
	return b.Bytes(), nil
}

// These are the components that can be configured
// through the "extra-config"
const (
	Apiserver         = "apiserver"
	ControllerManager = "controller-manager"
	Scheduler         = "scheduler"
	Etcd              = "etcd"
	Kubeadm           = "kubeadm"
	Kubeproxy         = "kube-proxy"
	Kubelet           = "kubelet"
)

// KubeadmExtraConfigOpts is a list of allowed "extra-config" components
var KubeadmExtraConfigOpts = []string{
	Apiserver,
	ControllerManager,
	Scheduler,
	Etcd,
	Kubeadm,
	Kubelet,
	Kubeproxy,
}

// InvokeKubeadm returns the invocation command for Kubeadm
func InvokeKubeadm(version string) string {
	return fmt.Sprintf("sudo env PATH=\"%s:$PATH\" kubeadm", binRoot(version))
}

// EtcdDataDir is where etcd data is stored.
func EtcdDataDir() string {
	return path.Join(vmpath.GuestPersistentDir, "etcd")
}

func etcdExtraArgs(extraOpts config.ExtraOptionSlice) map[string]string {
	args := map[string]string{}
	for _, eo := range extraOpts {
		if eo.Component != Etcd {
			continue
		}
		args[eo.Key] = eo.Value
	}
	return args
}

// HasResolvConfSearchRegression returns if the k8s version includes https://github.com/kubernetes/kubernetes/pull/109441
func HasResolvConfSearchRegression(k8sVersion string) bool {
	versionSemver, err := util.ParseKubernetesVersion(k8sVersion)
	if err != nil {
		klog.Warningf("was unable to parse Kubernetes version %q: %v", k8sVersion, err)
		return false
	}
	return versionSemver.EQ(semver.Version{Major: 1, Minor: 25})
}

// kubeletConfigOpts extracts only those kubelet extra options allowed by kubeletConfigParams.
func kubeletConfigOpts(extraOpts config.ExtraOptionSlice) map[string]string {
	args := map[string]string{}
	for _, eo := range extraOpts {
		if eo.Component != Kubelet {
			continue
		}
		if config.ContainsParam(kubeletConfigParams, eo.Key) {
			args[eo.Key] = eo.Value
		}
	}
	return args
}
