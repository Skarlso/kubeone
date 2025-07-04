/*
Copyright 2020 The KubeOne Authors.

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

package addons

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/BurntSushi/toml"
	"github.com/Masterminds/sprig/v3"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	kubeoneapi "k8c.io/kubeone/pkg/apis/kubeone"
	"k8c.io/kubeone/pkg/certificate/cabundle"
	"k8c.io/kubeone/pkg/fail"
	"k8c.io/kubeone/pkg/state"
	"k8c.io/kubeone/pkg/templates/resources"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

const (
	ParamsEnvPrefix = "env:"
)

func (a *applier) getManifestsFromDirectory(st *state.State, fsys fs.FS, addonName string) (string, error) {
	var (
		addonParams       map[string]string
		disableTemplating = false
	)
	if st.Cluster.Addons.Enabled() {
		for _, addon := range st.Cluster.Addons.OnlyAddons() {
			if addon.Name == addonName {
				addonParams = addon.Params
				disableTemplating = addon.DisableTemplating

				break
			}
		}
	}

	manifests, err := a.loadAddonsManifests(fsys, addonName, addonParams, st.Logger, st.Verbose, st.Cluster, disableTemplating)
	if err != nil {
		return "", err
	}

	addonsToMutate := sets.NewString(
		append(
			resources.CloudAddons(),
			resources.AddonBackupsRestic,
			resources.AddonOperatingSystemManager,
			resources.AddonMachineController,
		)...,
	)

	if !disableTemplating && addonsToMutate.Has(addonName) {
		if st.Cluster.CABundle != "" {
			if err = mutatePodTemplateSpec(manifests, func(podTpl *corev1.PodTemplateSpec) {
				cabundle.Inject(st.Cluster.CABundle, podTpl)
			}); err != nil {
				return "", err
			}
		}

		if st.Cluster.CloudProvider.SecretProviderClassName != "" {
			if err = mutatePodTemplateSpec(manifests, func(podSpec *corev1.PodTemplateSpec) {
				volume := corev1.Volume{
					Name: "secrets-store",
					VolumeSource: corev1.VolumeSource{
						CSI: &corev1.CSIVolumeSource{
							Driver:   "secrets-store.csi.k8s.io",
							ReadOnly: ptr.To(true),
							VolumeAttributes: map[string]string{
								"secretProviderClass": st.Cluster.CloudProvider.SecretProviderClassName,
							},
						},
					},
				}

				volumeMount := corev1.VolumeMount{
					Name:      "secrets-store",
					MountPath: "/mnt/secrets-store",
					ReadOnly:  true,
				}

				podSpec.Spec.Volumes = append(podSpec.Spec.Volumes, volume)
				for i := range podSpec.Spec.Containers {
					podSpec.Spec.Containers[i].VolumeMounts = append(podSpec.Spec.Containers[i].VolumeMounts, volumeMount)
				}
			}); err != nil {
				return "", err
			}
		}
	}

	rawManifests, err := ensureAddonsLabelsOnResources(manifests, addonName)
	if err != nil {
		return "", err
	}

	combinedManifests := combineManifests(rawManifests, disableTemplating)

	return combinedManifests.String(), nil
}

// loadAddonsManifests loads all YAML files from a given directory and runs the templating logic
func (a *applier) loadAddonsManifests(
	fsys fs.FS,
	addonName string,
	addonParams map[string]string,
	logger logrus.FieldLogger,
	verbose bool,
	k1cluster *kubeoneapi.KubeOneCluster,
	disableTemplating bool,
) ([]runtime.RawExtension, error) {
	var manifests []runtime.RawExtension

	files, err := fs.ReadDir(fsys, filepath.Join(".", addonName))
	if err != nil {
		return nil, fail.Runtime(err, "reading addons directory")
	}

	for _, file := range files {
		filePath := filepath.Join(addonName, file.Name())
		if file.IsDir() {
			continue
		}

		ext := strings.ToLower(filepath.Ext(file.Name()))
		// Only YAML, YML and JSON manifests are supported
		switch ext {
		case ".yaml", ".yml", ".json":
		default:
			if verbose {
				logger.Infof("Skipping file %q because it's not .yaml/.yml/.json file\n", file.Name())
			}

			continue
		}

		if verbose {
			logger.Debugf("Parsing addons manifest '%s'\n", file.Name())
		}

		manifestBytes, err := fs.ReadFile(fsys, filePath)
		if err != nil {
			return nil, fail.Runtime(err, "reading addon")
		}

		// We need to escape occurrences of Go text template in the manifests.
		if disableTemplating {
			res := strings.ReplaceAll(string(manifestBytes), "{{", "^{{")
			res = strings.ReplaceAll(res, "}}", "}}^")
			manifestBytes = []byte(res)
		}

		var manifest *bytes.Buffer
		manifest = bytes.NewBuffer(manifestBytes)

		if !disableTemplating {
			overwriteRegistry := k1cluster.RegistryConfiguration.ImageRegistry("")

			tpl, err := template.New("addons-base").
				Funcs(txtFuncMap(overwriteRegistry)).
				Funcs(template.FuncMap{
					"CABundle": func() string {
						return k1cluster.CertificateAuthority.Bundle
					},
				}).
				Parse(string(manifestBytes))
			if err != nil {
				return nil, fail.Runtime(err, "parsing addons manifest template %q", file.Name())
			}

			// Make a copy and merge Params
			tplDataParams := map[string]string{}
			for k, v := range a.TemplateData.Params {
				tplDataParams[k] = v
			}
			for k, v := range addonParams {
				tplDataParams[k] = v
			}

			// Resolve environment variables in Params
			for k, v := range tplDataParams {
				if strings.HasPrefix(v, ParamsEnvPrefix) {
					envName := strings.TrimPrefix(v, ParamsEnvPrefix)
					if env, ok := os.LookupEnv(envName); ok {
						tplDataParams[k] = env
					} else {
						return nil, fail.RuntimeError{
							Op:  "resolving template data environment variables",
							Err: fmt.Errorf("%q not found", envName),
						}
					}
				}
			}

			tplData := a.TemplateData
			tplData.Params = tplDataParams

			manifest = bytes.NewBuffer([]byte{})
			if err := tpl.Execute(manifest, tplData); err != nil {
				return nil, fail.Runtime(err, "executing addons manifest template %q", file.Name())
			}

			if len(bytes.TrimSpace(manifest.Bytes())) == 0 {
				logger.Infof("Addons manifest %q is empty after parsing. Skipping.\n", file.Name())
			}
		}

		reader := kyaml.NewYAMLReader(bufio.NewReader(manifest))
		for {
			yamlDoc, err := reader.Read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				return nil, fail.Runtime(err, "reading YAML reader for manifest %q", file.Name())
			}

			yamlDoc = bytes.TrimSpace(yamlDoc)
			if len(yamlDoc) == 0 {
				continue
			}

			decoder := kyaml.NewYAMLToJSONDecoder(bytes.NewBuffer(yamlDoc))
			raw := runtime.RawExtension{}
			if err := decoder.Decode(&raw); err != nil {
				return nil, fail.Runtime(err, "unmarshalling manifest %q", file.Name())
			}

			if len(raw.Raw) == 0 {
				// This can happen if the manifest contains only comments
				continue
			}

			manifests = append(manifests, raw)
		}
	}

	return manifests, nil
}

// ensureAddonsLabelsOnResources applies the addons label on all resources in the manifest
func ensureAddonsLabelsOnResources(manifests []runtime.RawExtension, addonName string) ([]*bytes.Buffer, error) {
	var rawManifests []*bytes.Buffer

	for _, rawManifest := range manifests {
		parsedUnstructuredObj := &metav1unstructured.Unstructured{}
		if _, _, err := metav1unstructured.UnstructuredJSONScheme.Decode(rawManifest.Raw, nil, parsedUnstructuredObj); err != nil {
			return nil, fail.Runtime(err, "parsing unstructured fields")
		}

		existingLabels := parsedUnstructuredObj.GetLabels()
		if existingLabels == nil {
			existingLabels = map[string]string{}
		}
		existingLabels[addonLabel] = addonName
		parsedUnstructuredObj.SetLabels(existingLabels)

		jsonBuffer := &bytes.Buffer{}
		if err := metav1unstructured.UnstructuredJSONScheme.Encode(parsedUnstructuredObj, jsonBuffer); err != nil {
			return nil, fail.Runtime(err, "marshalling unstructured fields")
		}

		// Must be encoded back to YAML, otherwise kubectl fails to apply because it tries to parse the whole
		// thing as json
		yamlBytes, err := yaml.JSONToYAML(jsonBuffer.Bytes())
		if err != nil {
			return nil, fail.Runtime(err, "recoding JSON to YAML")
		}

		rawManifests = append(rawManifests, bytes.NewBuffer(yamlBytes))
	}

	return rawManifests, nil
}

// combineManifests combines all manifest into a single one.
// This is needed so we can properly utilize kubectl apply --prune
func combineManifests(manifests []*bytes.Buffer, disablingTemplating bool) *bytes.Buffer {
	parts := make([]string, len(manifests))
	for i, m := range manifests {
		s := m.String()

		if disablingTemplating {
			// When templating is disabled we append "^" to avoid YAML to JSON conversion issues. We revert that change here.
			s = strings.ReplaceAll(s, "^{{", "{{")
			s = strings.ReplaceAll(s, "}}^", "}}")
		}

		s = strings.TrimSuffix(s, "\n")
		s = strings.TrimSpace(s)
		parts[i] = s
	}

	return bytes.NewBufferString(strings.Join(parts, "\n---\n") + "\n")
}

type vsphereCSIWebhookConfig struct {
	Port     string `toml:"port"`
	CertFile string `toml:"cert-file"`
	KeyFile  string `toml:"key-file"`
}

type vsphereCSIWebhookConfigWrapper struct {
	WebHookConfig vsphereCSIWebhookConfig `toml:"WebHookConfig"`
}

func txtFuncMap(overwriteRegistry string) template.FuncMap {
	funcs := sprig.TxtFuncMap()

	funcs["Registry"] = func(registry string) string {
		if overwriteRegistry != "" {
			return overwriteRegistry
		}

		return registry
	}

	funcs["required"] = requiredTemplateFunc
	funcs["caBundleEnvVar"] = caBundleEnvVarTemplateFunc
	funcs["caBundleVolume"] = caBundleVolumeTemplateFunc
	funcs["caBundleVolumeMount"] = caBundleVolumeMountTemplateFunc
	funcs["EquinixMetalSecret"] = equinixMetalSecretTemplateFunc
	funcs["vSphereCSIWebhookConfig"] = vSphereCSIWebhookConfigTemplateFunc

	return funcs
}

func requiredTemplateFunc(warn string, input interface{}) (interface{}, error) {
	switch val := input.(type) {
	case nil:
		return val, errors.New(warn)
	case string:
		if val == "" {
			return val, errors.New(warn)
		}
	}

	return input, nil
}

func caBundleEnvVarTemplateFunc() (string, error) {
	buf, err := yaml.Marshal([]corev1.EnvVar{cabundle.EnvVar()})

	return string(buf), err
}

func caBundleVolumeTemplateFunc() (string, error) {
	buf, err := yaml.Marshal([]corev1.Volume{cabundle.Volume()})

	return string(buf), err
}

func caBundleVolumeMountTemplateFunc() (string, error) {
	buf, err := yaml.Marshal([]corev1.VolumeMount{cabundle.VolumeMount()})

	return string(buf), err
}

func equinixMetalSecretTemplateFunc(apiKey, projectID string) (string, error) {
	equinixMetalSecret := struct {
		APIKey    string `json:"apiKey"`
		ProjectID string `json:"projectID"`
	}{
		APIKey:    apiKey,
		ProjectID: projectID,
	}

	buf, err := json.Marshal(equinixMetalSecret)

	return string(buf), err
}

func vSphereCSIWebhookConfigTemplateFunc() (string, error) {
	cfg := vsphereCSIWebhookConfigWrapper{
		WebHookConfig: vsphereCSIWebhookConfig{
			Port:     "8443",
			CertFile: "/run/secrets/tls/cert.pem",
			KeyFile:  "/run/secrets/tls/key.pem",
		},
	}

	var buf strings.Builder
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	err := enc.Encode(cfg)

	return buf.String(), err
}

func mutatePodTemplateSpec(docs []runtime.RawExtension, mutatorFn func(podTpl *corev1.PodTemplateSpec)) error {
	for i := range docs {
		ubject := metav1unstructured.Unstructured{}
		_, _, err := metav1unstructured.UnstructuredJSONScheme.Decode(docs[i].Raw, nil, &ubject)
		if err != nil {
			return err
		}

		switch ubject.GroupVersionKind().GroupKind() {
		case appsv1.SchemeGroupVersion.WithKind("Deployment").GroupKind():
			var obj appsv1.Deployment
			err = repackObject(&obj, &docs[i], func() {
				mutatorFn(&obj.Spec.Template)
			})
		case appsv1.SchemeGroupVersion.WithKind("StatefulSet").GroupKind():
			var obj appsv1.StatefulSet
			err = repackObject(&obj, &docs[i], func() {
				mutatorFn(&obj.Spec.Template)
			})
		case appsv1.SchemeGroupVersion.WithKind("DaemonSet").GroupKind():
			var obj appsv1.DaemonSet
			err = repackObject(&obj, &docs[i], func() {
				mutatorFn(&obj.Spec.Template)
			})
		case appsv1.SchemeGroupVersion.WithKind("ReplicaSet").GroupKind():
			var obj appsv1.ReplicaSet
			err = repackObject(&obj, &docs[i], func() {
				mutatorFn(&obj.Spec.Template)
			})
		case batchv1.SchemeGroupVersion.WithKind("Job").GroupKind():
			var obj batchv1.Job
			err = repackObject(&obj, &docs[i], func() {
				mutatorFn(&obj.Spec.Template)
			})
		case batchv1.SchemeGroupVersion.WithKind("CronJob").GroupKind():
			var obj batchv1.CronJob
			err = repackObject(&obj, &docs[i], func() {
				mutatorFn(&obj.Spec.JobTemplate.Spec.Template)
			})
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func repackObject(kubeobject runtime.Object, obj *runtime.RawExtension, mutator func()) error {
	if err := yaml.Unmarshal(obj.Raw, kubeobject); err != nil {
		return err
	}

	mutator()

	buf, err := yaml.Marshal(kubeobject)
	if err != nil {
		return err
	}

	js, err := yaml.YAMLToJSON(buf)
	if err != nil {
		return err
	}

	obj.Raw = js

	return nil
}
