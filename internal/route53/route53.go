package route53

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmTypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const (
	// SSM parameter prefix for storing instance registration data
	ssmParameterPrefix = "/smart-git-proxy/instances/"
)

// InstanceData stored in SSM for Lambda deregistration
type InstanceData struct {
	PrivateIP    string `json:"private_ip"`
	RecordName   string `json:"record_name"`
	HostedZoneID string `json:"hosted_zone_id"`
}

// Manager handles Route53 DNS registration
type Manager struct {
	hostedZoneID string
	recordName   string
	instanceID   string
	privateIP    string
	r53Client    *route53.Client
	ssmClient    *ssm.Client
	logger       *slog.Logger
}

// New creates a Route53 manager. It fetches EC2 instance metadata.
func New(ctx context.Context, hostedZoneID, recordName string, logger *slog.Logger) (*Manager, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	imdsClient := imds.NewFromConfig(cfg)

	instanceID, err := getInstanceID(ctx, imdsClient)
	if err != nil {
		return nil, fmt.Errorf("get instance id: %w", err)
	}

	privateIP, err := getPrivateIP(ctx, imdsClient)
	if err != nil {
		return nil, fmt.Errorf("get private ip: %w", err)
	}

	region, err := getRegion(ctx, imdsClient)
	if err != nil {
		return nil, fmt.Errorf("get region: %w", err)
	}

	// Reload config with region
	cfg, err = config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config with region: %w", err)
	}

	return &Manager{
		hostedZoneID: hostedZoneID,
		recordName:   recordName,
		instanceID:   instanceID,
		privateIP:    privateIP,
		r53Client:    route53.NewFromConfig(cfg),
		ssmClient:    ssm.NewFromConfig(cfg),
		logger:       logger,
	}, nil
}

// Register creates a multivalue A record and stores instance data in SSM
func (m *Manager) Register(ctx context.Context) error {
	// Create the DNS record
	_, err := m.r53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(m.hostedZoneID),
		ChangeBatch: &types.ChangeBatch{
			Comment: aws.String(fmt.Sprintf("Register instance %s", m.instanceID)),
			Changes: []types.Change{{
				Action: types.ChangeActionUpsert,
				ResourceRecordSet: &types.ResourceRecordSet{
					Name:             aws.String(m.recordName),
					Type:             types.RRTypeA,
					TTL:              aws.Int64(10), // Low TTL for faster failover
					SetIdentifier:    aws.String(m.instanceID),
					MultiValueAnswer: aws.Bool(true),
					ResourceRecords: []types.ResourceRecord{{
						Value: aws.String(m.privateIP),
					}},
				},
			}},
		},
	})
	if err != nil {
		return fmt.Errorf("create dns record: %w", err)
	}

	m.logger.Info("registered dns record",
		"name", m.recordName,
		"ip", m.privateIP,
		"instance_id", m.instanceID,
		"hosted_zone_id", m.hostedZoneID,
	)

	// Store instance data in SSM for Lambda deregistration
	data := InstanceData{
		PrivateIP:    m.privateIP,
		RecordName:   m.recordName,
		HostedZoneID: m.hostedZoneID,
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal instance data: %w", err)
	}

	paramName := ssmParameterPrefix + m.instanceID
	_, err = m.ssmClient.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(paramName),
		Value:     aws.String(string(dataJSON)),
		Type:      ssmTypes.ParameterTypeString,
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("store ssm parameter: %w", err)
	}

	m.logger.Info("stored instance data in ssm", "parameter", paramName)

	return nil
}

// Deregister removes the DNS record and SSM parameter
func (m *Manager) Deregister(ctx context.Context) error {
	// Delete the DNS record
	_, err := m.r53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(m.hostedZoneID),
		ChangeBatch: &types.ChangeBatch{
			Comment: aws.String(fmt.Sprintf("Deregister instance %s", m.instanceID)),
			Changes: []types.Change{{
				Action: types.ChangeActionDelete,
				ResourceRecordSet: &types.ResourceRecordSet{
					Name:             aws.String(m.recordName),
					Type:             types.RRTypeA,
					TTL:              aws.Int64(10),
					SetIdentifier:    aws.String(m.instanceID),
					MultiValueAnswer: aws.Bool(true),
					ResourceRecords: []types.ResourceRecord{{
						Value: aws.String(m.privateIP),
					}},
				},
			}},
		},
	})
	if err != nil {
		m.logger.Error("failed to delete dns record", "err", err)
	} else {
		m.logger.Info("deleted dns record", "instance_id", m.instanceID)
	}

	// Delete SSM parameter
	paramName := ssmParameterPrefix + m.instanceID
	_, ssmErr := m.ssmClient.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(paramName),
	})
	if ssmErr != nil {
		m.logger.Error("failed to delete ssm parameter", "err", ssmErr)
	} else {
		m.logger.Info("deleted ssm parameter", "parameter", paramName)
	}

	return err
}

func getInstanceID(ctx context.Context, client *imds.Client) (string, error) {
	output, err := client.GetMetadata(ctx, &imds.GetMetadataInput{
		Path: "instance-id",
	})
	if err != nil {
		return "", err
	}
	defer output.Content.Close()
	b, err := io.ReadAll(output.Content)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func getPrivateIP(ctx context.Context, client *imds.Client) (string, error) {
	output, err := client.GetMetadata(ctx, &imds.GetMetadataInput{
		Path: "local-ipv4",
	})
	if err != nil {
		return "", err
	}
	defer output.Content.Close()
	b, err := io.ReadAll(output.Content)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func getRegion(ctx context.Context, client *imds.Client) (string, error) {
	output, err := client.GetMetadata(ctx, &imds.GetMetadataInput{
		Path: "placement/region",
	})
	if err != nil {
		return getRegionFromDocument(ctx, client)
	}
	defer output.Content.Close()
	b, err := io.ReadAll(output.Content)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func getRegionFromDocument(ctx context.Context, client *imds.Client) (string, error) {
	output, err := client.GetMetadata(ctx, &imds.GetMetadataInput{
		Path: "dynamic/instance-identity/document",
	})
	if err != nil {
		return "", err
	}
	defer output.Content.Close()
	var doc struct {
		Region string `json:"region"`
	}
	if err := json.NewDecoder(output.Content).Decode(&doc); err != nil {
		return "", err
	}
	return doc.Region, nil
}
