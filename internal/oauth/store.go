package oauth

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const persistedStoreVersion = 1

type persistedStore struct {
	Version   int                `json:"version"`
	Issuer    string             `json:"issuer"`
	OwnerHash string             `json:"owner_hash"`
	Families  []persistedFamily  `json:"families"`
	Access    []persistedAccess  `json:"access"`
	Refresh   []persistedRefresh `json:"refresh"`
}

type persistedFamily struct {
	ID               string    `json:"id"`
	ClientID         string    `json:"client_id"`
	ClientSecretHash string    `json:"client_secret_hash"`
	Expires          time.Time `json:"expires"`
}

type persistedAccess struct {
	Hash    string    `json:"hash"`
	Family  string    `json:"family"`
	Expires time.Time `json:"expires"`
}

type persistedRefresh struct {
	Hash   string `json:"hash"`
	Family string `json:"family"`
	Used   bool   `json:"used,omitempty"`
}

func encodeDigest(d [32]byte) string {
	return hex.EncodeToString(d[:])
}

func decodeDigest(s string) ([32]byte, bool) {
	var out [32]byte
	if len(s) != 64 {
		return out, false
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return out, false
	}
	copy(out[:], raw)
	return out, true
}

// loadGrants restores unexpired hashed OAuth grant families, access digests, and
// refresh digests (including already-used markers for RFC 9700 replay detection)
// from disk. Any change to issuer, owner_token, or a client's secret invalidates
// the corresponding persisted grants automatically.
func (s *Server) loadGrants() {
	if s.storePath == "" {
		return
	}
	raw, err := os.ReadFile(s.storePath)
	if err != nil || len(raw) == 0 {
		return
	}
	var snapshot persistedStore
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return
	}
	if snapshot.Version != persistedStoreVersion || snapshot.Issuer != s.issuer {
		return
	}
	ownerDigest, ok := decodeDigest(snapshot.OwnerHash)
	if !ok || subtle.ConstantTimeCompare(ownerDigest[:], s.owner[:]) != 1 {
		return
	}

	now := s.now()
	for _, f := range snapshot.Families {
		if len(s.families) >= familyLimit || !now.Before(f.Expires) {
			continue
		}
		client, exists := s.clients[f.ClientID]
		if !exists {
			continue
		}
		secretDigest, ok := decodeDigest(f.ClientSecretHash)
		if !ok || subtle.ConstantTimeCompare(secretDigest[:], client.secret[:]) != 1 {
			continue
		}
		id, ok := decodeDigest(f.ID)
		if !ok {
			continue
		}
		s.families[id] = grantFamily{clientID: f.ClientID, expires: f.Expires}
	}

	for _, a := range snapshot.Access {
		if !now.Before(a.Expires) {
			continue
		}
		hash, okHash := decodeDigest(a.Hash)
		familyID, okFam := decodeDigest(a.Family)
		if !okHash || !okFam {
			continue
		}
		if _, exists := s.families[familyID]; !exists {
			continue
		}
		s.access[hash] = accessGrant{family: familyID, expires: a.Expires}
	}

	for _, r := range snapshot.Refresh {
		if len(s.refresh) >= refreshLimit {
			break
		}
		hash, okHash := decodeDigest(r.Hash)
		familyID, okFam := decodeDigest(r.Family)
		if !okHash || !okFam {
			continue
		}
		if _, exists := s.families[familyID]; !exists {
			continue
		}
		s.refresh[hash] = refreshGrant{family: familyID, used: r.Used}
	}
}

// persistLocked atomically writes the SHA-256-only grant state to s.storePath.
// Caller must hold s.mu. Never writes plaintext tokens, codes, or secrets.
func (s *Server) persistLocked(now time.Time) {
	if s.storePath == "" {
		return
	}
	s.cleanup(now)

	snapshot := persistedStore{
		Version:   persistedStoreVersion,
		Issuer:    s.issuer,
		OwnerHash: encodeDigest(s.owner),
		Families:  make([]persistedFamily, 0, len(s.families)),
		Access:    make([]persistedAccess, 0, len(s.access)),
		Refresh:   make([]persistedRefresh, 0, len(s.refresh)),
	}
	for id, fam := range s.families {
		client, exists := s.clients[fam.clientID]
		if !exists {
			continue
		}
		snapshot.Families = append(snapshot.Families, persistedFamily{
			ID:               encodeDigest(id),
			ClientID:         fam.clientID,
			ClientSecretHash: encodeDigest(client.secret),
			Expires:          fam.expires.UTC(),
		})
	}
	for h, a := range s.access {
		snapshot.Access = append(snapshot.Access, persistedAccess{
			Hash:    encodeDigest(h),
			Family:  encodeDigest(a.family),
			Expires: a.expires.UTC(),
		})
	}
	for h, r := range s.refresh {
		snapshot.Refresh = append(snapshot.Refresh, persistedRefresh{
			Hash:   encodeDigest(h),
			Family: encodeDigest(r.family),
			Used:   r.used,
		})
	}

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.storePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".lrmcp-oauth-grants-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := replaceFile(tmpName, s.storePath); err != nil {
		return
	}
	committed = true
}
