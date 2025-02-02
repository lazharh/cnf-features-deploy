package siteConfig

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	machineconfigv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	mcfgctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"

	"github.com/lack/yamltrim"
	yaml "gopkg.in/yaml.v3"
)

const McName = "predefined-extra-manifests"
const mcKind = "MachineConfig"

// annotationWarning is a helper to create warning annotation
// The `values` field should contain a key for the specific CR you wish to apply a warning to
// and the struct will associated to that CR key will simply contain the specific thing you wish to warn about
// and a brief message about the warning.
type annotationWarning struct {
	annoKey string
	values  map[string][]annotationValue
	mutex   sync.RWMutex
}

type annotationValue struct {
	fieldName    string
	fieldMessage string
}

type AnnotationMessage struct {
	CRName string
	annotationValue
	ShouldBeApplied func(Clusters) bool
}

var annotationMessages = []AnnotationMessage{
	{
		CRName: "AgentClusterInstall",
		annotationValue: annotationValue{
			fieldName:    "cpuset",
			fieldMessage: "cpuset will be deprecated after OCP 4.15, please use cpuPartitioningMode for OCP versions >= 4.14",
		},
		ShouldBeApplied: func(c Clusters) bool {
			for _, node := range c.Nodes {
				if node.Cpuset != "" {
					return true
				}
			}
			return false
		},
	},

	{
		CRName: "ConfigMap",
		annotationValue: annotationValue{
			fieldName:    "extraManifestPath",
			fieldMessage: "extraManifestPath will be deprecated after OCP 4.15, please use ExtraManifests.SearchPaths for OCP versions >= 4.14",
		},
		ShouldBeApplied: func(c Clusters) bool {
			if len(c.ExtraManifestPath) > 0 {
				return true
			}
			return false
		},
	},
}

func NewAnnotationWarning(annoKey string) *annotationWarning {
	return &annotationWarning{
		annoKey: fmt.Sprintf("%s-%s", ZtpWarningAnnotation, annoKey),
	}
}

func (d *annotationWarning) init() {
	if d.values == nil {
		d.values = map[string][]annotationValue{}
	}
}

func (d *annotationWarning) Add(crName, field, message string) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.init()
	d.values[crName] = append(d.values[crName], annotationValue{fieldName: field, fieldMessage: message})
}

func (d *annotationWarning) GetAnnotationKey(val annotationValue) string {
	return fmt.Sprintf("%s-%s", d.annoKey, val.fieldName)
}

func (d *annotationWarning) GetValue(crKey string) (value []annotationValue, found bool) {
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	d.init()
	value, found = d.values[crKey]
	return
}

func (d *annotationWarning) HasWarnings() bool {
	return len(d.values) > 0
}

// merge the spec fields of all MC manifests except the ones that are in the doNotMerge list
func MergeManifests(individualMachineConfigs map[string]interface{}, doNotMerge map[string]bool) (map[string]interface{}, error) {
	// key is role, value is a list of MCs
	mergableMachineConfigs := make(map[string][]*machineconfigv1.MachineConfig)

	for filename, machineConfig := range individualMachineConfigs {
		if doNotMerge[filename] {
			continue
		}

		var data map[string]interface{}
		if err := yaml.Unmarshal([]byte(machineConfig.(string)), &data); err != nil {
			log.Printf("Error Could not unmarshal file content: (%s): %s\n", data, err)
			return individualMachineConfigs, err
		}
		// skip the manifest that is not a machine config
		if data["kind"] != mcKind {
			continue
		}

		err := addMachineConfig(data, mergableMachineConfigs)
		if err != nil {
			return individualMachineConfigs, err
		}
		// remove the individual file entries
		delete(individualMachineConfigs, filename)
	}

	for roleName, machineConfigs := range mergableMachineConfigs {
		cconfig := &machineconfigv1.ControllerConfig{}
		//It only uses OSImageURL provided by the CVO
		merged, err := mcfgctrlcommon.MergeMachineConfigs(machineConfigs, cconfig)
		if err != nil {
			return nil, err
		}

		merged.SetName(fmt.Sprintf("%s-%s", McName, roleName))
		merged.ObjectMeta.Labels = make(map[string]string)
		merged.ObjectMeta.Labels[machineconfigv1.MachineConfigRoleLabelKey] = roleName
		merged.ObjectMeta.Annotations = make(map[string]string)
		merged.ObjectMeta.Annotations[ZtpAnnotation] = ZtpAnnotationDefaultValue
		merged.TypeMeta.APIVersion = machineconfigv1.GroupVersion.String()
		merged.TypeMeta.Kind = mcKind

		// Marshal the machine config to json string
		b, err := json.Marshal(merged)
		if err != nil {
			log.Printf("Error: could not convert mc to json: (%s)\n", err)
			return nil, err
		}

		var m map[string]interface{}
		// Unmarshal the json string to interface for YamlTrim
		err = json.Unmarshal(b, &m)
		if err != nil {
			log.Printf("Error: could not convert json to map: (%s): %s\n", b, err)
			return nil, err
		}

		d := yamltrim.YamlTrim(m)
		if d == nil {
			return nil, fmt.Errorf("empty machineconfig")
		}
		// Marshal the interface to yaml bytes
		yamlBytes, err := yaml.Marshal(d)
		if err != nil {
			log.Printf("Error: could not convert map to yaml: (%s): %s\n", m, err)
			return nil, err
		}
		fileName := fmt.Sprintf("%s.yaml", merged.ObjectMeta.Name)
		individualMachineConfigs[fileName] = string(yamlBytes)
	}

	return individualMachineConfigs, nil
}

// convert yaml data to MC
func convertToMC(data map[string]interface{}) (*machineconfigv1.MachineConfig, error) {
	// Convert the yaml string to json
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("Error: could not convert map to json: (%s): %s\n", data, err)
		return nil, err
	}
	mc := machineconfigv1.MachineConfig{}
	// Convert the json string to machine config struct
	err = json.Unmarshal(jsonData, &mc)
	if err != nil {
		log.Printf("Error: could not convert json to mc: (%s): %s\n", jsonData, err)
		return nil, err
	}
	return &mc, nil
}

func addMachineConfig(data map[string]interface{}, configs map[string][]*machineconfigv1.MachineConfig) error {
	mc, err := convertToMC(data)
	if err != nil {
		return err
	}
	role := mc.ObjectMeta.Labels[machineconfigv1.MachineConfigRoleLabelKey]
	if configs[role] != nil {
		configs[role] = append(configs[role], mc)
	} else {
		configs[role] = []*machineconfigv1.MachineConfig{mc}
	}
	return nil
}

func addZTPAnnotation(data map[string]interface{}, extraAnnotations ...*annotationWarning) {

	if data["metadata"] == nil {
		data["metadata"] = make(map[string]interface{})
	}

	if data["metadata"].(map[string]interface{})["annotations"] == nil {
		data["metadata"].(map[string]interface{})["annotations"] = make(map[string]interface{})
	}
	// A dynamic value might be added later
	data["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})[ZtpAnnotation] = ZtpAnnotationDefaultValue

	for _, annotation := range extraAnnotations {
		if annotation.HasWarnings() {
			if val, ok := data["kind"].(string); ok {
				if warnings, found := annotation.GetValue(val); found {
					for _, message := range warnings {
						data["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})[annotation.GetAnnotationKey(message)] = message.fieldMessage
					}
				}
			}
		}
	}
}

// Add ztp deploy annotation to all siteconfig generated CRs
func addZTPAnnotationToCRs(clusterCRs []interface{}, extraAnnotations ...*annotationWarning) ([]interface{}, error) {

	for _, v := range clusterCRs {
		addZTPAnnotation(v.(map[string]interface{}), extraAnnotations...)
	}
	return clusterCRs, nil
}

// Add ztp deploy annotation to a manifest
func addZTPAnnotationToManifest(manifestStr string) (string, error) {

	var data map[string]interface{}
	err := yaml.Unmarshal([]byte(manifestStr), &data)
	if err != nil {
		log.Printf("Error: could not unmarshal string:(%+v) (%s)\n", manifestStr, err)
		return manifestStr, err
	}

	addZTPAnnotation(data)
	out, err := yaml.Marshal(data)
	if err != nil {
		log.Printf("Error: could not marshal data:(%+v) (%s)\n", data, err)
		return manifestStr, err
	}
	return string(out), nil
}

func deleteInspectAnnotation(bmhCR map[string]interface{}) map[string]interface{} {
	metadata, _ := bmhCR["metadata"].(map[string]interface{})
	annotations, _ := metadata["annotations"].(map[string]interface{})

	if inspect, ok := annotations[inspectAnnotationPrefix]; ok && inspect != inspectDisabled {
		delete(annotations, inspectAnnotationPrefix)
	}
	return bmhCR
}

// agentClusterInstallAnnotation returns string in json format
func agentClusterInstallAnnotation(networkType, installConfigOverrides string) (string, error) {

	var commonKey = "networking"
	networkAnnotation := "{\"networking\":{\"networkType\":\"" + networkType + "\"}}"
	if !json.Valid([]byte(networkAnnotation)) {
		return "", fmt.Errorf("Invalid json conversion of network type")
	}

	switch installConfigOverrides {
	case "":
		return networkAnnotation, nil

	default:
		if !json.Valid([]byte(installConfigOverrides)) {
			return "", fmt.Errorf("Invalid json parameter set at installConfigOverride")
		}

		var installConfigOverridesMap map[string]interface{}
		err := json.Unmarshal([]byte(installConfigOverrides), &installConfigOverridesMap)
		if err != nil {
			return "", fmt.Errorf("Could not unmarshal installConfigOverrides data: %v\n", installConfigOverrides)
		}

		if _, found := installConfigOverridesMap[commonKey]; found {
			networkMergedJson, err := mergeJsonCommonKey(networkAnnotation, installConfigOverrides, commonKey)
			if err != nil {
				return "", fmt.Errorf("Couldn't marshal annotation for AgentClusterInstall, Error: %v\n", err)
			}
			return networkMergedJson, nil
		}

		trimmedConfigOverrides := strings.TrimPrefix(installConfigOverrides, "{")
		trimmedNetworkType := strings.TrimSuffix(networkAnnotation, "}")
		finalJson := trimmedNetworkType + "," + trimmedConfigOverrides
		if !json.Valid([]byte(finalJson)) {
			return "", fmt.Errorf("Couldn't marshal annotation for AgentClusterInstall")
		}
		return finalJson, nil

	}

}

// mergeJsonCommonKey merge 2 json in common key and return string
func mergeJsonCommonKey(mergeWith, mergeTo, key string) (string, error) {

	var (
		networkAnnotation      map[string]interface{}
		installConfigOverrides map[string]interface{}
	)

	// converted to map
	err := json.Unmarshal([]byte(mergeWith), &networkAnnotation)
	if err != nil {
		return "", err
	}

	// converted to map
	err = json.Unmarshal([]byte(mergeTo), &installConfigOverrides)
	if err != nil {
		return "", err
	}

	// reate a new map which will be passed to networking
	// the size of the map can be anything but must be initialized
	// otherwise it will panic
	mergedValueMap := make(map[string]interface{}, len(installConfigOverrides))

	// append value to the new map
	if value, found := installConfigOverrides[key]; found {
		anothernConfig := value.(map[string]interface{})
		for i, v := range anothernConfig {
			mergedValueMap[i] = v
		}
	}

	// append the value to the new map
	// additionally if user passed a wrong value for
	// networkType as "networkType":"default", it will be
	// overwritten with correct value
	if value, found := networkAnnotation[key]; found {
		value := value.(map[string]interface{})
		for i, v := range value {
			mergedValueMap[i] = v
		}
	}

	// set networking field to the new map
	installConfigOverrides[key] = mergedValueMap

	// build new json and return as string
	newJson, err := json.Marshal(installConfigOverrides)
	if err != nil {
		return "", err
	}
	return string(newJson), nil
}

func applyWorkloadPinningInstallConfigOverrides(clusterSpec *Clusters) (result string, err error) {
	const (
		cpuPartitioningKey = "cpuPartitioningMode"
	)

	if clusterSpec.CPUPartitioning == CPUPartitioningAllNodes {
		installOverrideValues := map[string]interface{}{}
		if clusterSpec.InstallConfigOverrides != "" {
			err := json.Unmarshal([]byte(clusterSpec.InstallConfigOverrides), &installOverrideValues)
			if err != nil {
				fmt.Println("err", err)
				return clusterSpec.InstallConfigOverrides, err
			}
		}

		// Because the explicit value clusterSpec.CPUPartitioning == CPUPartitioningAllNodes, we always overwrite
		// the installConfigOverrides value or add it if not present
		installOverrideValues[cpuPartitioningKey] = CPUPartitioningAllNodes

		byteData, err := json.Marshal(installOverrideValues)
		if err != nil {
			return clusterSpec.InstallConfigOverrides, err
		}
		return string(byteData), nil
	}

	return clusterSpec.InstallConfigOverrides, nil
}

// getDeprecationWarnings is a helper function to add deprecation warning
func getDeprecationWarnings(clusterSpec Clusters) *annotationWarning {
	deprecationWarnings := NewAnnotationWarning(ZtpDeprecationWarningAnnotationPostfix)

	for _, field := range annotationMessages {
		if field.ShouldBeApplied != nil && field.ShouldBeApplied(clusterSpec) {
			deprecationWarnings.Add(field.CRName, field.annotationValue.fieldName, field.annotationValue.fieldMessage)
		}
	}
	return deprecationWarnings
}

// to apply a label 'node-role.kubernetes.io/environment: production' on a node
// the following annotation should be added to the BMH:
// bmac.agent-install.openshift.io.node-label.node-role.kubernetes.io/environment: production
func transformNodeLabelAnnotation(bmhCR map[string]interface{}) map[string]interface{} {
	metadata, _ := bmhCR["metadata"].(map[string]interface{})

	if label, ok := metadata["annotations"].(map[string]interface{})[nodeLabelPrefix]; ok {
		for k, v := range label.(map[string]string) {
			newKey := nodeLabelPrefix + "." + k
			metadata["annotations"].(map[string]interface{})[newKey] = v
		}

		delete(metadata["annotations"].(map[string]interface{}), nodeLabelPrefix)
	}

	return bmhCR
}
