package imoutput

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
)

func TestReferenceCapabilityBindsExactSourceAndTenant(t *testing.T) {
	signer := NewReferenceSigner(referenceTestKey)
	source := &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeID: "doc-1", ChunkID: "chunk-1",
	}
	token, err := signer.Issue(source, 7)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	capability, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if capability.TenantID != 7 || capability.KnowledgeBaseID != "kb-1" ||
		capability.KnowledgeID != "doc-1" || capability.ChunkID != "chunk-1" ||
		capability.Type != sourcerefs.SourceTypeKnowledge {
		t.Fatalf("unexpected capability: %#v", capability)
	}

	otherTenant, err := signer.Issue(source, 8)
	if err != nil || otherTenant == token {
		t.Fatalf("tenant must be part of the signed coordinate: token=%q err=%v", otherTenant, err)
	}
	second, err := signer.Issue(source, 7)
	if err != nil || second != token {
		t.Fatalf("durable IM links should be stable for the same exact source: %q %q err=%v", token, second, err)
	}
}

func TestReferenceCapabilityRejectsTamperingWrongKeyAndUnsupportedTypes(t *testing.T) {
	signer := NewReferenceSigner(referenceTestKey)
	token, err := signer.Issue(&sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeWiki, KnowledgeBaseID: "kb-1", Slug: "concept/rag",
	}, 7)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	encoded, signature, ok := strings.Cut(token, ".")
	if !ok || signature == "" {
		t.Fatalf("unexpected issued token: %q", token)
	}
	const rawURLAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(rawURLAlphabet, signature[len(signature)-1])
	if last < 0 || last%4 != 0 || last+1 >= len(rawURLAlphabet) {
		t.Fatalf("unexpected canonical signature tail: %q", signature)
	}
	// A 32-byte signature has two unused low bits in its final Base64URL
	// character. Go's decoder accepts a different textual character with the
	// same significant bits unless verification also enforces canonical form.
	nonCanonicalEquivalent := encoded + "." + signature[:len(signature)-1] + string(rawURLAlphabet[last+1])
	for _, candidate := range []string{
		token[:len(token)-1] + "A",
		nonCanonicalEquivalent,
		token + ".extra",
		"",
		strings.Repeat("x", maxReferenceTokenLength+1),
	} {
		if _, err := signer.Verify(candidate); err == nil {
			t.Fatalf("tampered token accepted: %q", candidate)
		}
	}
	if _, err := NewReferenceSigner([]byte("abcdef0123456789abcdef0123456789")).Verify(token); err == nil {
		t.Fatal("token signed by another deployment key was accepted")
	}
	if _, err := signer.Issue(&sourcerefs.CitationSource{Type: sourcerefs.SourceTypeWeb, URL: "https://example.com"}, 7); err == nil {
		t.Fatal("external web URLs must not be wrapped in an internal public capability")
	}
	if _, err := NewReferenceSigner(nil).Issue(&sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeWiki, KnowledgeBaseID: "kb", Slug: "page",
	}, 7); err == nil {
		t.Fatal("missing signing key must fail closed")
	}
}

func TestReferenceCapabilityParserRejectsUnknownFieldsEvenWithValidMAC(t *testing.T) {
	signer := NewReferenceSigner(referenceTestKey)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"t":"wiki","n":7,"k":"kb","s":"page","redirect":"https://evil.example"}`))
	token := payload + "." + base64.RawURLEncoding.EncodeToString(signer.sign(payload))
	if _, err := signer.Verify(token); err == nil {
		t.Fatal("unknown capability fields were accepted")
	}
}
