package console

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

const vaultMagic = "UBQV1"

// Connection is one encrypted BYOK provider profile and the public model
// aliases exposed through it. Secret fields are never serialized by handlers.
type Connection struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	ProviderType string         `json:"provider_type"`
	APIBase      string         `json:"api_base,omitempty"`
	APIVersion   string         `json:"api_version,omitempty"`
	Project      string         `json:"project,omitempty"`
	Location     string         `json:"location,omitempty"`
	Region       string         `json:"region,omitempty"`
	APIKey       string         `json:"api_key,omitempty"`
	APISecret    string         `json:"api_secret,omitempty"`
	SessionToken string         `json:"session_token,omitempty"`
	Models       []ManagedModel `json:"models"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

type ManagedModel struct {
	Name                 string  `json:"name"`
	ProviderModel        string  `json:"provider_model"`
	IsEU                 bool    `json:"is_eu"`
	Restricted           bool    `json:"restricted"`
	InputCostPerMillion  float64 `json:"input_cost_per_million,omitempty"`
	OutputCostPerMillion float64 `json:"output_cost_per_million,omitempty"`
}

type ConnectionView struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	ProviderType string         `json:"provider_type"`
	APIBase      string         `json:"api_base,omitempty"`
	APIVersion   string         `json:"api_version,omitempty"`
	Project      string         `json:"project,omitempty"`
	Location     string         `json:"location,omitempty"`
	Region       string         `json:"region,omitempty"`
	HasAPIKey    bool           `json:"has_api_key"`
	HasSecret    bool           `json:"has_secret"`
	Models       []ManagedModel `json:"models"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

type vaultDocument struct {
	Version     int          `json:"version"`
	Connections []Connection `json:"connections"`
}

// Vault stores the whole credential document under AES-256-GCM. Keeping the
// metadata encrypted too avoids leaking provider accounts and endpoint names
// from a stolen appliance disk.
type Vault struct {
	mu   sync.RWMutex
	path string
	aead cipher.AEAD
	doc  vaultDocument
}

func OpenVault(path, keyFile string) (*Vault, error) {
	key, err := loadOrCreateVaultKey(keyFile)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault gcm: %w", err)
	}
	v := &Vault{path: path, aead: aead, doc: vaultDocument{Version: 1}}
	if err := v.load(); err != nil {
		return nil, err
	}
	return v, nil
}

func loadOrCreateVaultKey(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("vault key file is required")
	}
	if data, err := os.ReadFile(path); err == nil {
		decoded, decodeErr := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr != nil || len(decoded) != 32 {
			return nil, errors.New("vault key file must contain one base64 encoded 256-bit key")
		}
		return decoded, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read vault key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create vault key directory: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate vault key: %w", err)
	}
	encoded := []byte(base64.RawStdEncoding.EncodeToString(key) + "\n")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create vault key: %w", err)
	}
	if _, err = f.Write(encoded); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write vault key: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close vault key: %w", err)
	}
	return key, nil
}

func (v *Vault) load() error {
	data, err := os.ReadFile(v.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read credential vault: %w", err)
	}
	if len(data) < len(vaultMagic)+v.aead.NonceSize() || string(data[:len(vaultMagic)]) != vaultMagic {
		return errors.New("credential vault has an invalid format")
	}
	nonce := data[len(vaultMagic) : len(vaultMagic)+v.aead.NonceSize()]
	plain, err := v.aead.Open(nil, nonce, data[len(vaultMagic)+v.aead.NonceSize():], []byte(vaultMagic))
	if err != nil {
		return errors.New("credential vault cannot be decrypted with the configured key")
	}
	if err := json.Unmarshal(plain, &v.doc); err != nil {
		return fmt.Errorf("decode credential vault: %w", err)
	}
	if v.doc.Version != 1 {
		return fmt.Errorf("unsupported credential vault version %d", v.doc.Version)
	}
	return nil
}

func (v *Vault) saveLocked() error {
	plain, err := json.Marshal(v.doc)
	if err != nil {
		return fmt.Errorf("encode credential vault: %w", err)
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate vault nonce: %w", err)
	}
	sealed := v.aead.Seal(nil, nonce, plain, []byte(vaultMagic))
	data := append(append([]byte(vaultMagic), nonce...), sealed...)
	if err := os.MkdirAll(filepath.Dir(v.path), 0o700); err != nil {
		return fmt.Errorf("create vault directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(v.path), ".credentials-*")
	if err != nil {
		return fmt.Errorf("create temporary vault: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write credential vault: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, v.path); err != nil {
		return fmt.Errorf("replace credential vault: %w", err)
	}
	return nil
}

func (v *Vault) List() []ConnectionView {
	v.mu.RLock()
	defer v.mu.RUnlock()
	result := make([]ConnectionView, 0, len(v.doc.Connections))
	for _, c := range v.doc.Connections {
		result = append(result, viewOf(c))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (v *Vault) Connections() []Connection {
	v.mu.RLock()
	defer v.mu.RUnlock()
	result := make([]Connection, len(v.doc.Connections))
	copy(result, v.doc.Connections)
	return result
}

func (v *Vault) Create(c Connection) (ConnectionView, error) {
	if err := validateConnection(c); err != nil {
		return ConnectionView{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, existing := range v.doc.Connections {
		if strings.EqualFold(existing.Name, c.Name) {
			return ConnectionView{}, errors.New("a connection with this name already exists")
		}
	}
	c.ID = randomID()
	c.CreatedAt = time.Now().UTC()
	c.UpdatedAt = c.CreatedAt
	v.doc.Connections = append(v.doc.Connections, c)
	if err := v.saveLocked(); err != nil {
		v.doc.Connections = v.doc.Connections[:len(v.doc.Connections)-1]
		return ConnectionView{}, err
	}
	return viewOf(c), nil
}

func (v *Vault) Delete(id string) (Connection, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, c := range v.doc.Connections {
		if c.ID != id {
			continue
		}
		previous := append([]Connection(nil), v.doc.Connections...)
		v.doc.Connections = append(v.doc.Connections[:i], v.doc.Connections[i+1:]...)
		if err := v.saveLocked(); err != nil {
			v.doc.Connections = previous
			return Connection{}, err
		}
		return c, nil
	}
	return Connection{}, errors.New("connection not found")
}

func (c Connection) ModelConfigs() []config.ModelConfig {
	models := make([]config.ModelConfig, 0, len(c.Models))
	for _, model := range c.Models {
		models = append(models, config.ModelConfig{
			Name: model.Name, Provider: c.ProviderType, ProviderProfile: "byok-" + c.ID,
			ProviderModel: model.ProviderModel, APIBase: c.APIBase, APIKey: c.APIKey,
			APISecret: c.APISecret, APIVersion: c.APIVersion, Project: c.Project,
			Location: c.Location, Region: c.Region, SessionToken: c.SessionToken,
			IsEU: model.IsEU, Restricted: model.Restricted,
			InputCostPerMillion: model.InputCostPerMillion, OutputCostPerMillion: model.OutputCostPerMillion,
			// BYOK changes who pays the provider, not whether Ubiquum accounts
			// and enforces the request cost. Managed connections stay metered so
			// an identity's hard budget remains effective.
			AuthMode: config.AuthModeAPIKey, BillingMode: config.BillingModeMetered,
		})
	}
	return models
}

func validateConnection(c Connection) error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("connection name is required")
	}
	if strings.TrimSpace(c.ProviderType) == "" {
		return errors.New("provider_type is required")
	}
	if len(c.Models) == 0 {
		return errors.New("at least one model is required")
	}
	seen := map[string]bool{}
	for _, model := range c.Models {
		if strings.TrimSpace(model.Name) == "" || strings.TrimSpace(model.ProviderModel) == "" {
			return errors.New("every model requires name and provider_model")
		}
		if seen[model.Name] {
			return fmt.Errorf("duplicate model name %q", model.Name)
		}
		seen[model.Name] = true
	}
	return nil
}

func viewOf(c Connection) ConnectionView {
	return ConnectionView{
		ID: c.ID, Name: c.Name, ProviderType: c.ProviderType, APIBase: c.APIBase,
		APIVersion: c.APIVersion, Project: c.Project, Location: c.Location, Region: c.Region,
		HasAPIKey: c.APIKey != "", HasSecret: c.APISecret != "" || c.SessionToken != "",
		Models: c.Models, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
