package virtual

import (
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/json"
	"testing"
	"time"
)

func TestReadClusterInfo(t *testing.T) {
	clusterInfoPath := "/tmp"
	readInitClusterInfo(clusterInfoPath)
}

func TestLoadAutoScalerConfig(t *testing.T) {
	expectedConfig := gst.AutoScalerConfig{
		NodeTemplates: map[string]gst.NodeTemplate{
			"a": {
				Name:             "a",
				CPU:              gst.MustParseQuantity("10Mi"),
				GPU:              gst.MustParseQuantity("12Mi"),
				Memory:           gst.MustParseQuantity("10Gi"),
				EphemeralStorage: gst.MustParseQuantity("11Gi"),
				InstanceType:     "m5.large",
				Region:           "eu-west-1",
				Zone:             "eu-west-1a",
			},
		},
		NodeGroups: map[string]gst.NodeGroupInfo{
			"a": {
				Name:       "a",
				PoolName:   "p1",
				Zone:       "eu-west-1a",
				TargetSize: 2,
				MinSize:    1,
				MaxSize:    5,
			},
		},
		/*WorkerPools: []gst.WorkerPoolInfo{
			{
				Architecture: "arm64",
				Minimum:      1,
				Maximum:      5,
				MaxSurge: intstr.IntOrString{
					Type:   intstr.String,
					StrVal: "10%",
				},
				MaxUnavailable: intstr.IntOrString{
					Type:   intstr.String,
					StrVal: "15%",
				},
				Zones: []string{"eu-west-1a"},
			},
		},*/
		CASettings: gst.CASettingsInfo{
			Expander: "least-waste",
			NodeGroupsMinMax: map[string]gst.MinMax{
				"a": gst.MinMax{
					Min: 1,
					Max: 5,
				},
			},
			MaxNodeProvisionTime:          10 * time.Minute,
			ScanInterval:                  10 * time.Second,
			MaxGracefulTerminationSeconds: 10,
			NewPodScaleUpDelay:            5,
			MaxEmptyBulkDelete:            2,
			IgnoreDaemonSetUtilization:    false,
			MaxNodesTotal:                 10,
			Priorities:                    "dummy",
		},
	}
	bytes, err := json.Marshal(expectedConfig)
	assert.Nil(t, err)
	var actualLoadedConfig gst.AutoScalerConfig
	err = json.Unmarshal(bytes, &actualLoadedConfig)
	assert.Nil(t, err)
	assert.Equal(t, expectedConfig, actualLoadedConfig)

}
