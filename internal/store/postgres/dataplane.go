package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lllmml/production-go-llm-gateway/internal/dataplane"
	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

func (s *Store) AuthenticateVirtualKey(ctx context.Context, prefix string, keyHash []byte) (dataplane.AuthContext, error) {
	stored, err := s.queries.AuthenticateVirtualAPIKey(ctx, AuthenticateVirtualAPIKeyParams{
		KeyPrefix: prefix,
		KeyHash:   keyHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return dataplane.AuthContext{}, dataplane.ErrNotFound
	}
	if err != nil {
		return dataplane.AuthContext{}, err
	}
	return dataplane.AuthContext{
		ProjectID:    formatUUID(stored.ProjectID),
		VirtualKeyID: formatUUID(stored.ID),
		KeyPrefix:    stored.KeyPrefix,
	}, nil
}

func (s *Store) ResolveProviderCredential(ctx context.Context, projectID string, providerName provider.Name) (dataplane.ProviderCredential, error) {
	selectedProjectID, err := parseUUID(projectID)
	if err != nil {
		return dataplane.ProviderCredential{}, dataplane.ErrNotFound
	}
	stored, err := s.queries.GetActiveProviderCredentialForProjectConfig(ctx, GetActiveProviderCredentialForProjectConfigParams{
		ProjectID: selectedProjectID,
		Provider:  string(providerName),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return dataplane.ProviderCredential{}, dataplane.ErrNotFound
	}
	if err != nil {
		return dataplane.ProviderCredential{}, err
	}
	return dataplane.ProviderCredential{
		ID:               formatUUID(stored.CredentialID),
		ProjectID:        formatUUID(stored.ProjectID),
		Provider:         provider.Name(stored.Provider),
		SecretCiphertext: stored.SecretCiphertext,
		SecretNonce:      stored.SecretNonce,
		KeyVersion:       stored.KeyVersion,
		BaseURLOverride:  textValue(stored.BaseUrlOverride),
	}, nil
}

func (s *Store) CreateGatewayRequest(ctx context.Context, params dataplane.CreateRequestParams) (dataplane.GatewayRequest, error) {
	id, err := newUUID()
	if err != nil {
		return dataplane.GatewayRequest{}, err
	}
	projectID, virtualKeyID, credentialID, err := requestIdentity(params)
	if err != nil {
		return dataplane.GatewayRequest{}, err
	}
	stored, err := s.queries.CreateGatewayRequest(ctx, CreateGatewayRequestParams{
		ID:                   id,
		ProjectID:            projectID,
		VirtualKeyID:         virtualKeyID,
		ProviderCredentialID: credentialID,
		Provider:             string(params.Provider),
		Model:                params.Model,
		IsStream:             params.IsStream,
		StartedAt:            timestamptz(params.StartedAt),
		TraceID:              optionalText(params.TraceID),
	})
	if err != nil {
		return dataplane.GatewayRequest{}, err
	}
	return dataplane.GatewayRequest{
		ID:                   formatUUID(stored.ID),
		ProjectID:            formatUUID(stored.ProjectID),
		VirtualKeyID:         formatUUID(stored.VirtualKeyID),
		ProviderCredentialID: formatUUID(stored.ProviderCredentialID),
		Provider:             provider.Name(stored.Provider),
		Model:                stored.Model,
		IsStream:             stored.IsStream,
		Status:               stored.Status,
		StartedAt:            stored.StartedAt.Time,
	}, nil
}

func (s *Store) FindModelPrice(ctx context.Context, providerName provider.Name, model string, at time.Time) (dataplane.ModelPrice, error) {
	stored, err := s.queries.FindModelPriceAt(ctx, FindModelPriceAtParams{
		Provider: string(providerName),
		Model:    model,
		AtTime:   timestamptz(at),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return dataplane.ModelPrice{}, dataplane.ErrNotFound
	}
	if err != nil {
		return dataplane.ModelPrice{}, err
	}
	return dataplane.ModelPrice{
		ID:                      formatUUID(stored.ID),
		InputNanoUSDPerMillion:  stored.InputNanoUsdPerMillion,
		OutputNanoUSDPerMillion: stored.OutputNanoUsdPerMillion,
	}, nil
}

func (s *Store) FinalizeGatewayRequest(ctx context.Context, params dataplane.FinalizeParams) error {
	id, err := parseUUID(params.ID)
	if err != nil {
		return dataplane.ErrNotFound
	}
	_, err = s.queries.FinalizeGatewayRequest(ctx, FinalizeGatewayRequestParams{
		ID:                   id,
		Status:               params.Status,
		FirstChunkAt:         optionalTimestamptz(params.FirstChunkAt),
		CompletedAt:          timestamptz(params.CompletedAt),
		LatencyMs:            optionalInt8(params.LatencyMS),
		TtftMs:               optionalInt8(params.TTFTMS),
		UpstreamHttpStatus:   optionalInt4(params.UpstreamHTTPStatus),
		ErrorCategory:        optionalErrorCategory(params.ErrorCategory),
		RetryCount:           params.RetryCount,
		PromptTokens:         optionalInt8(params.PromptTokens),
		CompletionTokens:     optionalInt8(params.CompletionTokens),
		TotalTokens:          optionalInt8(params.TotalTokens),
		UsageSource:          optionalString(params.UsageSource),
		PricingID:            optionalUUID(params.PricingID),
		EstimatedCostNanoUsd: optionalInt8(params.EstimatedCostNanoUSD),
		UpstreamRequestID:    optionalString(params.UpstreamRequestID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return dataplane.ErrNotFound
	}
	return err
}

func requestIdentity(params dataplane.CreateRequestParams) (pgtype.UUID, pgtype.UUID, pgtype.UUID, error) {
	projectID, err := parseUUID(params.ProjectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("parse gateway request project ID: %w", err)
	}
	virtualKeyID, err := parseUUID(params.VirtualKeyID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("parse gateway request virtual key ID: %w", err)
	}
	credentialID, err := parseUUID(params.ProviderCredentialID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("parse gateway request credential ID: %w", err)
	}
	return projectID, virtualKeyID, credentialID, nil
}

func optionalInt8(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}

func optionalInt4(value *int32) pgtype.Int4 {
	if value == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *value, Valid: true}
}

func optionalTimestamptz(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return timestamptz(*value)
}

func optionalString(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *value, Valid: true}
}

func optionalErrorCategory(value *provider.ErrorCategory) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: string(*value), Valid: true}
}

func optionalUUID(value *string) pgtype.UUID {
	if value == nil {
		return pgtype.UUID{}
	}
	id, err := parseUUID(*value)
	if err != nil {
		return pgtype.UUID{}
	}
	return id
}
