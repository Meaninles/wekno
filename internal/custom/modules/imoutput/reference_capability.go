package imoutput

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
)

const (
	referenceCapabilityVersion = 1
	referenceCapabilityDomain  = "weknora:im-reference:v1\x00"
	maxReferenceTokenLength    = 2048
	maxReferencePayloadLength  = 1024
)

var (
	ErrReferenceSigningKeyUnavailable = errors.New("IM reference signing key is unavailable")
	ErrInvalidReferenceCapability     = errors.New("invalid IM reference capability")
)

// ReferenceCapability is an unforgeable, read-only coordinate issued only at
// the final IM boundary. It deliberately carries no arbitrary URL and cannot
// be used to enumerate a tenant or knowledge base.
type ReferenceCapability struct {
	Version         int    `json:"v"`
	Type            string `json:"t"`
	TenantID        uint64 `json:"n"`
	KnowledgeBaseID string `json:"k"`
	KnowledgeID     string `json:"d,omitempty"`
	ChunkID         string `json:"c,omitempty"`
	Slug            string `json:"s,omitempty"`
}

// ReferenceSigner signs device-neutral IM citation capabilities. Tokens are
// intentionally durable: historical IM messages must remain readable. A
// capability stops resolving as soon as its exact source, owning document, or
// knowledge base is disabled, archived, or deleted.
type ReferenceSigner struct {
	key []byte
}

func NewReferenceSigner(key []byte) *ReferenceSigner {
	if len(key) != 32 {
		return &ReferenceSigner{}
	}
	return &ReferenceSigner{key: append([]byte(nil), key...)}
}

func NewReferenceSignerFromEnv() *ReferenceSigner {
	return NewReferenceSigner([]byte(os.Getenv("SYSTEM_AES_KEY")))
}

func (s *ReferenceSigner) Issue(source *sourcerefs.CitationSource, tenantID uint64) (string, error) {
	if s == nil || len(s.key) != 32 {
		return "", ErrReferenceSigningKeyUnavailable
	}
	capability, err := capabilityFromSource(source, tenantID)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(capability)
	if err != nil {
		return "", fmt.Errorf("marshal IM reference capability: %w", err)
	}
	if len(payload) > maxReferencePayloadLength {
		return "", ErrInvalidReferenceCapability
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := s.sign(encoded)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (s *ReferenceSigner) Verify(token string) (ReferenceCapability, error) {
	if s == nil || len(s.key) != 32 {
		return ReferenceCapability{}, ErrReferenceSigningKeyUnavailable
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxReferenceTokenLength || strings.Count(token, ".") != 1 {
		return ReferenceCapability{}, ErrInvalidReferenceCapability
	}
	encoded, signatureText, _ := strings.Cut(token, ".")
	if encoded == "" || signatureText == "" {
		return ReferenceCapability{}, ErrInvalidReferenceCapability
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureText)
	if err != nil ||
		base64.RawURLEncoding.EncodeToString(signature) != signatureText ||
		len(signature) != sha256.Size ||
		!hmac.Equal(s.sign(encoded), signature) {
		return ReferenceCapability{}, ErrInvalidReferenceCapability
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil ||
		base64.RawURLEncoding.EncodeToString(payload) != encoded ||
		len(payload) == 0 ||
		len(payload) > maxReferencePayloadLength {
		return ReferenceCapability{}, ErrInvalidReferenceCapability
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var capability ReferenceCapability
	if err := decoder.Decode(&capability); err != nil {
		return ReferenceCapability{}, ErrInvalidReferenceCapability
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ReferenceCapability{}, ErrInvalidReferenceCapability
	}
	if err := validateReferenceCapability(capability); err != nil {
		return ReferenceCapability{}, err
	}
	return capability, nil
}

func (s *ReferenceSigner) sign(encodedPayload string) []byte {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(referenceCapabilityDomain))
	_, _ = mac.Write([]byte(encodedPayload))
	return mac.Sum(nil)
}

func capabilityFromSource(source *sourcerefs.CitationSource, tenantID uint64) (ReferenceCapability, error) {
	if source == nil || tenantID == 0 {
		return ReferenceCapability{}, ErrInvalidReferenceCapability
	}
	capability := ReferenceCapability{
		Version:         referenceCapabilityVersion,
		Type:            strings.TrimSpace(source.Type),
		TenantID:        tenantID,
		KnowledgeBaseID: strings.TrimSpace(source.KnowledgeBaseID),
		KnowledgeID:     strings.TrimSpace(source.KnowledgeID),
		ChunkID:         strings.TrimSpace(source.ChunkID),
		Slug:            strings.TrimSpace(source.Slug),
	}
	if err := validateReferenceCapability(capability); err != nil {
		return ReferenceCapability{}, err
	}
	return capability, nil
}

func validateReferenceCapability(capability ReferenceCapability) error {
	if capability.Version != referenceCapabilityVersion ||
		capability.TenantID == 0 ||
		!boundedCoordinate(capability.KnowledgeBaseID, 128) {
		return ErrInvalidReferenceCapability
	}
	switch capability.Type {
	case sourcerefs.SourceTypeKnowledge:
		if !boundedCoordinate(capability.KnowledgeID, 128) ||
			!boundedCoordinate(capability.ChunkID, 128) ||
			capability.Slug != "" {
			return ErrInvalidReferenceCapability
		}
	case sourcerefs.SourceTypeWiki:
		if !boundedCoordinate(capability.Slug, 512) || capability.KnowledgeID != "" || capability.ChunkID != "" {
			return ErrInvalidReferenceCapability
		}
	default:
		return ErrInvalidReferenceCapability
	}
	return nil
}

func boundedCoordinate(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\r\n\x00")
}
