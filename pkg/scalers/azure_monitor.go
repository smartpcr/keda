package scalers

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/preview/monitor/mgmt/2018-03-01/insights"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"k8s.io/klog"
)

// Much of the code in this file is taken from the Azure Kubernetes Metrics Adapter
// https://github.com/Azure/azure-k8s-metrics-adapter/tree/master/pkg/azure/externalmetrics

type azureExternalMetricRequest struct {
	MetricName                string
	SubscriptionID            string
	ResourceName              string
	ResourceProviderNamespace string
	ResourceType              string
	Aggregation               string
	Timespan                  string
	Filter                    string
	ResourceGroup             string
}

// GetAzureMetricValue returns the value of an Azure Monitor metric, rounded to the nearest int
func GetAzureMetricValue(ctx context.Context, metricMetadata *azureMonitorMetadata) (int32, error) {
	client := createMetricsClient(metricMetadata)

	requestPtr, err := createMetricsRequest(metricMetadata)
	if err != nil {
		return -1, err
	}

	return executeRequest(client, requestPtr)
}

func createMetricsClient(metadata *azureMonitorMetadata) insights.MetricsClient {
	client := insights.NewMetricsClient(metadata.subscriptionID)
	config := auth.NewClientCredentialsConfig(metadata.clientID, metadata.clientPassword, metadata.tenantID)

	authorizer, _ := config.Authorizer()
	client.Authorizer = authorizer

	return client
}

func createMetricsRequest(metadata *azureMonitorMetadata) (*azureExternalMetricRequest, error) {
	metricRequest := azureExternalMetricRequest{
		MetricName:     metadata.name,
		SubscriptionID: metadata.subscriptionID,
		Aggregation:    metadata.aggregationType,
		Filter:         metadata.filter,
		ResourceGroup:  metadata.resourceGroupName,
	}

	resourceInfo := strings.Split(metadata.resourceURI, "/")
	metricRequest.ResourceProviderNamespace = resourceInfo[0]
	metricRequest.ResourceType = resourceInfo[1]
	metricRequest.ResourceName = resourceInfo[2]

	// if no timespan is provided, defaults to 5 minutes
	timespan, err := formatTimeSpan(metadata.aggregationInterval)
	if err != nil {
		return nil, err
	}

	metricRequest.Timespan = timespan

	return &metricRequest, nil
}

func executeRequest(client insights.MetricsClient, request *azureExternalMetricRequest) (int32, error) {
	metricResponse, err := getAzureMetric(client, *request)
	if err != nil {
		azureMonitorLog.Error(err, "error getting azure monitor metric")
		return -1, fmt.Errorf("Error getting azure monitor metric %s: %s", request.MetricName, err.Error())
	}

	// casting drops everything after decimal, so round first
	metricValue := int32(math.Round(metricResponse))

	return metricValue, nil
}

func getAzureMetric(client insights.MetricsClient, azMetricRequest azureExternalMetricRequest) (float64, error) {
	err := azMetricRequest.validate()
	if err != nil {
		return -1, err
	}

	metricResourceURI := azMetricRequest.metricResourceURI()
	klog.V(2).Infof("resource uri: %s", metricResourceURI)

	metricResult, err := client.List(context.Background(), metricResourceURI,
		azMetricRequest.Timespan, nil,
		azMetricRequest.MetricName, azMetricRequest.Aggregation, nil,
		"", azMetricRequest.Filter, "", "")
	if err != nil {
		return -1, err
	}

	value, err := extractValue(azMetricRequest, metricResult)

	return value, err
}

func extractValue(azMetricRequest azureExternalMetricRequest, metricResult insights.Response) (float64, error) {
	metricVals := *metricResult.Value

	if len(metricVals) == 0 {
		err := fmt.Errorf("Got an empty response for metric %s/%s and aggregate type %s", azMetricRequest.ResourceProviderNamespace, azMetricRequest.MetricName, insights.AggregationType(strings.ToTitle(azMetricRequest.Aggregation)))
		return -1, err
	}

	timeseries := *metricVals[0].Timeseries
	if timeseries == nil {
		err := fmt.Errorf("Got metric result for %s/%s and aggregate type %s without timeseries", azMetricRequest.ResourceProviderNamespace, azMetricRequest.MetricName, insights.AggregationType(strings.ToTitle(azMetricRequest.Aggregation)))
		return -1, err
	}

	data := *timeseries[0].Data
	if data == nil {
		err := fmt.Errorf("Got metric result for %s/%s and aggregate type %s without any metric values", azMetricRequest.ResourceProviderNamespace, azMetricRequest.MetricName, insights.AggregationType(strings.ToTitle(azMetricRequest.Aggregation)))
		return -1, err
	}

	valuePtr, err := verifyAggregationTypeIsSupported(azMetricRequest.Aggregation, data)
	if err != nil {
		return -1, fmt.Errorf("Unable to get value for metric %s/%s with aggregation %s. No value returned by Azure Monitor", azMetricRequest.ResourceProviderNamespace, azMetricRequest.MetricName, azMetricRequest.Aggregation)
	}

	klog.V(2).Infof("metric type: %s %f", azMetricRequest.Aggregation, *valuePtr)

	return *valuePtr, nil
}

func (amr azureExternalMetricRequest) validate() error {
	if amr.MetricName == "" {
		return fmt.Errorf("metricName is required")
	}
	if amr.ResourceGroup == "" {
		return fmt.Errorf("resourceGroup is required")
	}
	if amr.SubscriptionID == "" {
		return fmt.Errorf("subscriptionID is required. set a default or pass via label selectors")
	}
	return nil
}

func (amr azureExternalMetricRequest) metricResourceURI() string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s/%s",
		amr.SubscriptionID,
		amr.ResourceGroup,
		amr.ResourceProviderNamespace,
		amr.ResourceType,
		amr.ResourceName)
}

// formatTimeSpan defaults to a 5 minute timespan if the user does not provide one
func formatTimeSpan(timeSpan string) (string, error) {
	endtime := time.Now().UTC().Format(time.RFC3339)
	starttime := time.Now().Add(-(5 * time.Minute)).UTC().Format(time.RFC3339)
	if timeSpan != "" {
		aggregationInterval := strings.Split(timeSpan, ":")
		hours, herr := strconv.Atoi(aggregationInterval[0])
		minutes, merr := strconv.Atoi(aggregationInterval[1])
		seconds, serr := strconv.Atoi(aggregationInterval[2])

		if herr != nil || merr != nil || serr != nil {
			return "", fmt.Errorf("Errors parsing metricAggregationInterval: %v, %v, %v", herr, merr, serr)
		}

		starttime = time.Now().Add(-(time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second)).UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s/%s", starttime, endtime), nil
}

func verifyAggregationTypeIsSupported(aggregationType string, data []insights.MetricValue) (*float64, error) {
	var valuePtr *float64
	if strings.EqualFold(string(insights.Average), aggregationType) && data[len(data)-1].Average != nil {
		valuePtr = data[len(data)-1].Average
	} else if strings.EqualFold(string(insights.Total), aggregationType) && data[len(data)-1].Total != nil {
		valuePtr = data[len(data)-1].Total
	} else if strings.EqualFold(string(insights.Maximum), aggregationType) && data[len(data)-1].Maximum != nil {
		valuePtr = data[len(data)-1].Maximum
	} else if strings.EqualFold(string(insights.Minimum), aggregationType) && data[len(data)-1].Minimum != nil {
		valuePtr = data[len(data)-1].Minimum
	} else if strings.EqualFold(string(insights.Count), aggregationType) && data[len(data)-1].Count != nil {
		fValue := float64(*data[len(data)-1].Count)
		valuePtr = &fValue
	} else {
		err := fmt.Errorf("Unsupported aggregation type %s", insights.AggregationType(strings.ToTitle(aggregationType)))
		return nil, err
	}
	return valuePtr, nil
}
