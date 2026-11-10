package admin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"golang.org/x/crypto/argon2"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"
	"github.com/ZhenForest/xsbot-core/internal/config"
)

type credential struct {
	Schema    int    `yaml:"schema_version"`
	Username  string `yaml:"username"`
	Salt      string `yaml:"salt"`
	Hash      string `yaml:"hash"`
	Temporary bool   `yaml:"temporary"`
}
type Identity struct {
	mu            sync.Mutex
	credential    credential
	path          string
	session, csrf string
	failures      map[string]int
	hashing       chan struct{}
}

func random(n int) string {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func passwordHash(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
}
func makeCredential(username, password string, temporary bool) (credential, error) {
	if username == "" {
		return credential{}, fmt.Errorf("username required")
	}
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return credential{}, e
	}
	return credential{Schema: 1, Username: username, Salt: base64.RawStdEncoding.EncodeToString(salt), Hash: base64.RawStdEncoding.EncodeToString(passwordHash(password, salt)), Temporary: temporary}, nil
}
func InitIdentity(root, username string) (*Identity, string, error) {
	dir := filepath.Join(root, "data/auth")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, "", e
	}
	path := filepath.Join(dir, "credentials.yaml")
	marker := filepath.Join(dir, "installed")
	markErr := installedMarker(marker)
	anchor := filepath.Join(root, ".auth-installed")
	anchorErr := installedMarker(anchor)
	_, fileErr := os.Stat(path)
	initial := os.IsNotExist(markErr) && os.IsNotExist(fileErr) && os.IsNotExist(anchorErr)
	var c credential
	var temporary string
	if initial {
		temporary = random(18)
		var e error
		c, e = makeCredential(username, temporary, true)
		if e != nil {
			return nil, "", e
		}
		b, _ := config.Encode(c)
		if e = config.WriteAtomic(path, b); e != nil {
			return nil, "", e
		}
		if e = config.WriteAtomic(marker, []byte("1\n")); e != nil {
			return nil, "", e
		}
		if e = config.WriteAtomic(anchor, []byte("1\n")); e != nil {
			return nil, "", e
		}
	} else {
		if markErr != nil || fileErr != nil || anchorErr != nil {
			return nil, "", fmt.Errorf("installed auth state incomplete; offline repair required")
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return nil, "", e
		}
		if e = config.DecodeStrict(b, &c); e != nil {
			return nil, "", e
		}
	}
	salt, e := base64.RawStdEncoding.DecodeString(c.Salt)
	if e != nil || len(salt) != 16 {
		return nil, "", fmt.Errorf("invalid auth salt")
	}
	hash, e := base64.RawStdEncoding.DecodeString(c.Hash)
	if e != nil || len(hash) != 32 || c.Schema != 1 || c.Username == "" {
		return nil, "", fmt.Errorf("invalid credential state")
	}
	return &Identity{credential: c, path: path, failures: map[string]int{}, hashing: make(chan struct{}, 1)}, temporary, nil
}
func normalizeIP(raw string) string {
	host, _, e := net.SplitHostPort(raw)
	if e == nil {
		raw = host
	}
	ip, e := netip.ParseAddr(raw)
	if e != nil {
		return raw
	}
	return ip.Unmap().String()
}
func (i *Identity) Login(ip, username, password string) (string, string, bool, error) {
	ip = normalizeIP(ip)
	i.mu.Lock()
	if i.failures[ip] >= 5 {
		i.mu.Unlock()
		return "", "", false, fmt.Errorf("login blocked for this IP until Core restart")
	}
	c := i.credential
	i.mu.Unlock()
	select {
	case i.hashing <- struct{}{}:
		defer func() { <-i.hashing }()
	default:
		return "", "", false, fmt.Errorf("password service busy; retry")
	}
	salt, _ := base64.RawStdEncoding.DecodeString(c.Salt)
	expected, _ := base64.RawStdEncoding.DecodeString(c.Hash)
	actual := passwordHash(password, salt)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.failures[ip] >= 5 {
		return "", "", false, fmt.Errorf("login blocked")
	}
	if c.Hash != i.credential.Hash || c.Username != i.credential.Username {
		return "", "", false, fmt.Errorf("account changed; retry")
	}
	if subtle.ConstantTimeCompare(actual, expected) != 1 || username != c.Username {
		i.failures[ip]++
		return "", "", false, fmt.Errorf("incorrect credentials")
	}
	delete(i.failures, ip)
	i.session = random(32)
	i.csrf = random(32)
	return i.session, i.csrf, c.Temporary, nil
}
func (i *Identity) Check(session string) (bool, bool, string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	ok := session != "" && subtle.ConstantTimeCompare([]byte(session), []byte(i.session)) == 1
	return ok, i.credential.Temporary, i.csrf
}
func (i *Identity) Logout() { i.mu.Lock(); defer i.mu.Unlock(); i.session = ""; i.csrf = "" }
func (i *Identity) Change(session, username, password string) error {
	if password == "" {
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.credential.Temporary {
			return fmt.Errorf("temporary password must be changed")
		}
		if session == "" || subtle.ConstantTimeCompare([]byte(session), []byte(i.session)) != 1 {
			return fmt.Errorf("session expired")
		}
		if username == "" {
			return fmt.Errorf("username required")
		}
		next := i.credential
		next.Username = username
		data, _ := config.Encode(next)
		if e := config.WriteAtomic(i.path, data); e != nil {
			return e
		}
		i.credential = next
		i.session = ""
		i.csrf = ""
		return nil
	}
	if utf8.RuneCountInString(password) < 12 || !utf8.ValidString(password) || username == "" {
		return fmt.Errorf("username required; password must have at least 12 characters")
	}
	select {
	case i.hashing <- struct{}{}:
		defer func() { <-i.hashing }()
	default:
		return fmt.Errorf("password service busy")
	}
	next, e := makeCredential(username, password, false)
	if e != nil {
		return e
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if session == "" || subtle.ConstantTimeCompare([]byte(session), []byte(i.session)) != 1 {
		return fmt.Errorf("session expired")
	}
	b, _ := config.Encode(next)
	if e = config.WriteAtomic(i.path, b); e != nil {
		return e
	}
	i.credential = next
	i.session = ""
	i.csrf = ""
	return nil
}
func ResetIdentity(root, username string) (string, error) {
	// Resetting the password must not restore an old template username after an
	// administrator renamed the installed account.
	if data, e := os.ReadFile(filepath.Join(root, "data/auth/credentials.yaml")); e == nil {
		var existing credential
		if config.DecodeStrict(data, &existing) == nil && existing.Schema == 1 && existing.Username != "" {
			username = existing.Username
		}
	} else if !os.IsNotExist(e) {
		return "", e
	}
	password := random(18)
	c, e := makeCredential(username, password, true)
	if e != nil {
		return "", e
	}
	dir := filepath.Join(root, "data/auth")
	if e = os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	b, _ := config.Encode(c)
	if e = config.WriteAtomic(filepath.Join(dir, "credentials.yaml"), b); e != nil {
		return "", e
	}
	if e = config.WriteAtomic(filepath.Join(dir, "installed"), []byte("1\n")); e != nil {
		return "", e
	}
	if e = config.WriteAtomic(filepath.Join(root, ".auth-installed"), []byte("1\n")); e != nil {
		return "", e
	}
	return password, nil
}
func installedMarker(path string) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if string(b) != "1\n" {
		return fmt.Errorf("invalid installation marker")
	}
	return nil
}
func Certificate(root, ip string, renew bool) (string, string, error) {
	address := net.ParseIP(ip)
	if address == nil {
		return "", "", fmt.Errorf("invalid public IP")
	}
	dir := filepath.Join(root, "data/tls")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", "", e
	}
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	marker := filepath.Join(dir, "installed")
	_, ce := os.Stat(cert)
	_, ke := os.Stat(key)
	me := installedMarker(marker)
	anchor := filepath.Join(root, ".tls-installed")
	ae := installedMarker(anchor)
	initial := os.IsNotExist(ce) && os.IsNotExist(ke) && os.IsNotExist(me) && os.IsNotExist(ae)
	if initial || renew {
		private, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			return "", "", e
		}
		serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if e != nil {
			return "", "", e
		}
		now := time.Now()
		template := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: ip}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0), IPAddresses: []net.IP{address}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
		der, e := x509.CreateCertificate(rand.Reader, &template, &template, &private.PublicKey, private)
		if e != nil {
			return "", "", e
		}
		raw, e := x509.MarshalPKCS8PrivateKey(private)
		if e != nil {
			return "", "", e
		}
		if e = config.WriteAtomic(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); e != nil {
			return "", "", e
		}
		if e = config.WriteAtomic(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})); e != nil {
			return "", "", e
		}
		if e = config.WriteAtomic(marker, []byte("1\n")); e != nil {
			return "", "", e
		}
		if e = config.WriteAtomic(anchor, []byte("1\n")); e != nil {
			return "", "", e
		}
	} else if ce != nil || ke != nil || me != nil || ae != nil {
		return "", "", fmt.Errorf("installed TLS state incomplete; offline repair required")
	}
	pair, e := tls.LoadX509KeyPair(cert, key)
	if e != nil {
		return "", "", e
	}
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		return "", "", e
	}
	if e = leaf.VerifyHostname(ip); e != nil {
		return "", "", e
	}
	if time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return "", "", fmt.Errorf("certificate expired or not valid yet")
	}
	return cert, key, nil
}
func TokenDigest(s string) string { sum := sha256.Sum256([]byte(s)); return fmt.Sprintf("%x", sum[:]) }
