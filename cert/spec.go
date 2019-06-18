// Package cert contains certificate specifications and
// certificate-specific management.
package cert

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/cloudflare/certmgr/metrics"
	"github.com/cloudflare/certmgr/svcmgr"
	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/transport"
	"github.com/cloudflare/cfssl/transport/core"
)

// A Spec contains information needed to monitor and renew a
// certificate.
type Spec struct {

	// This defines the service manager to use.  This should be defined
	// globally rather than per cert- it's allowed here to allow cert
	// definitions to use a servicemanager of 'command' to allow freeform
	// invocations.
	ServiceManagerName string `json:"svcmgr" yaml:"svcmgr"`

	serviceManager svcmgr.Manager

	// The service is the service that uses this certificate. If
	// this field is not empty, the action below will be applied
	// to this service upon certificate renewal. It can also be
	// used to describe what this certificate is for.
	Service string `json:"service" yaml:"service"`

	// Action is one of empty, "nop", "reload", or "restart" (see
	// the svcmgr package for details).
	Action string `json:"action" yaml:"action"`

	// Request contains the CSR metadata needed to request a
	// certificate.
	Request *csr.CertificateRequest `json:"request" yaml:"request"`

	// Key contains the file metadata for the private key.
	Key *File `json:"private_key" yaml:"private_key"`

	// Cert contains the file metadata for the certificate.
	Cert *File `json:"certificate" yaml:"certificate"`

	// CA specifies the certificate authority that should be used.
	CA CA `json:"authority" yaml:"authority"`

	// Path points to the on-disk location of the certificate
	// spec.
	Path string

	tr *transport.Transport
}

func (spec *Spec) String() string {
	extra := displayName(spec.Request.Name())
	if extra == "" {
		extra = spec.Service
	}

	if extra == "" {
		extra = spec.Cert.Path
	}
	if extra != "" {
		return fmt.Sprintf("spec: %s: %s", spec.Cert.Path, extra)
	}

	return fmt.Sprintf("spec: %s", spec.Cert.Path)
}

// Identity creates a transport package identity for the certificate.
func (spec *Spec) identity() (*core.Identity, error) {
	ident := &core.Identity{
		Request: spec.Request,
		Roots: []*core.Root{
			&core.Root{
				Type: "system",
			},
			&core.Root{
				Type: "cfssl",
				Metadata: map[string]string{
					"host":          spec.CA.Remote,
					"profile":       spec.CA.Profile,
					"label":         spec.CA.Label,
					"tls-remote-ca": spec.CA.RootCACert,
				},
			},
		},
		Profiles: map[string]map[string]string{
			"cfssl": map[string]string{
				"remote":        spec.CA.Remote,
				"profile":       spec.CA.Profile,
				"label":         spec.CA.Label,
				"tls-remote-ca": spec.CA.RootCACert,
			},
			"paths": map[string]string{
				"private_key": spec.Key.Path,
				"certificate": spec.Cert.Path,
			},
		},
	}

	authkey := spec.CA.AuthKey
	if spec.CA.AuthKeyFile != "" {
		log.Debugf("loading auth_key_file %v", spec.CA.AuthKeyFile)
		content, err := ioutil.ReadFile(spec.CA.AuthKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed reading auth_key_file %v: %v", spec.CA.AuthKeyFile, err)
		}
		authkey = strings.TrimSpace(string(content))
	}
	if authkey != "" {
		ident.Profiles["cfssl"]["auth-type"] = "standard"
		ident.Profiles["cfssl"]["auth-key"] = authkey
	}

	return ident, nil
}

func newSpecFromPath(path string, defaultServiceManager string) (*Spec, error) {
	in, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var spec = &Spec{
		Request: csr.New(),
		Path:    path,
	}

	switch filepath.Ext(path) {
	case ".json":
		err = json.Unmarshal(in, &spec)
	case ".yml", ".yaml":
		err = yaml.UnmarshalStrict(in, &spec)
	default:
		err = fmt.Errorf("cert: unrecognised spec file format for %s", path)
	}

	return spec, err
}

// Load reads a spec from a JSON configuration file.
func Load(path, remote string, before time.Duration, defaultServiceManager string, strict bool) (*Spec, error) {
	spec, err := newSpecFromPath(path, defaultServiceManager)
	if err != nil {
		return nil, err
	}

	if spec.CA.Remote == "" {
		spec.CA.Remote = remote
	}

	if spec.CA.Remote == "" {
		return nil, errors.New("cert: no remote specified in authority (either in the spec or in the certmgr config)")
	}

	err = spec.CA.Load()
	if err != nil {
		return nil, err
	}

	identity, err := spec.identity()
	if err != nil {
		return nil, err
	}
	spec.tr, err = transport.New(before, identity)
	if err != nil {
		return nil, err
	}

	// The provider's Load returning an error here just means that
	// the certificate and private key don't exist yet.
	err = spec.tr.Provider.Load()
	if err != nil {
		err = nil
	}

	manager, _ := svcmgr.New("dummy", "", "")
	if spec.Action != "" && spec.Action != "nop" {
		manager, err = svcmgr.New(spec.ServiceManagerName, spec.Action, spec.Service)
		if err != nil {
			return nil, err
		}
	}

	// If action is undefined and svcmgr isn't dummy, we will throw a warning due to likely undefined cert renewal behavior
	// We will refuse to even store/keep track of the cert if we're in strict mode
	if (spec.Action == "" || spec.Action == "nop") && (spec.ServiceManagerName != "" && spec.ServiceManagerName != "dummy") {
		log.Warningf("manager: No action defined for a non-dummy svcmgr in certificate spec. This can lead to undefined certificate renewal behavior.")
		if strict {
			return nil, nil
		}
	}
	spec.serviceManager = manager
	return spec, err
}

// RefreshKeys will make sure the key pair in the Spec has loaded keys
// and has a valid certificate. It will handle any persistence, check
// that the certificate is valid (i.e. that its expiry date is within
// the Before date), and handle certificate reissuance as needed.
func (spec *Spec) RefreshKeys() error {
	if spec.tr == nil {
		panic("cert: cannot refresh keys because spec has an invalid transport")
	}

	if !spec.tr.Provider.Persistent() {
		panic("cert: cannot manage ephemeral certificates")
	}

	err := spec.tr.RefreshKeys()
	if err != nil {
		return err
	}

	err = spec.Key.Set()
	if err != nil {
		return err
	}

	err = spec.Cert.Set()
	if err != nil {
		return err
	}

	return nil
}

// Ready returns true if the key pair specified by the Spec exists; it
// doesn't check whether it needs to be renewed.
func (spec *Spec) Ready() bool {
	if spec.tr == nil {
		panic("cert: cannot check readiness because spec has an invalid transport")
	}

	return spec.tr.Provider.Ready()
}

// Lifespan returns a time.Duration for the certificate's validity.
func (spec *Spec) Lifespan() time.Duration {
	if spec.tr == nil {
		panic("cert: cannot check certificate's lifespan because spec has an invalid transport")
	}

	// This bit of code is necessary to confirm that the cert/key are older than the spec definition.
	if spec.IsChangedOnDisk(spec.Key.Path) || spec.IsChangedOnDisk(spec.Cert.Path) {
		// This is necessary to essentially force cfssl to regenerate since it's not spec aware.
		log.Infof("refreshing due to spec %s having a newer mtime than key or cert", spec.Path)
		spec.ResetLifespan()
		return 0
	}
	return spec.tr.Lifespan()
}

// IsChangedOnDisk method to report if the given path is older than the spec
func (spec *Spec) IsChangedOnDisk(path string) bool {
	specStat, err := os.Stat(spec.Path)
	if err != nil {
		// The assertion here is that the spec actually
		// exists. If it doesn't, something is wrong with the
		// world.
		log.Warning("cert: IsChangedOnDisk: Spec file does not exist")
		return true
	}
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Errorf("cert isChangedOnDisk: while checking path %s, got path error %s", path, err)
		}
		return true
	}
	return specStat.ModTime().After(st.ModTime())
}

// CheckDiskPKI checks the PKI information on disk against cert spec and alerts upon differences
// Specifically, it checks that private key on disk matches spec algorithm & keysize,
// and certificate on disk matches CSR spec info
func (spec *Spec) CheckDiskPKI() error {
	certPath := spec.Cert.Path
	keyPath := spec.Key.Path
	csrRequest := spec.Request

	// Read private key algorithm and keysize from disk, determine if RSA or ECDSA
	keyData, err := ioutil.ReadFile(keyPath)
	if err != nil {
		return err
	}
	pemKey, _ := pem.Decode(keyData)
	if pemKey == nil {
		return errors.New("Unable to pem decode private key on disk")
	}

	var algDisk string
	var sizeDisk int
	privKey, err := x509.ParsePKCS1PrivateKey(pemKey.Bytes)
	if err != nil {
		privKey, err := x509.ParseECPrivateKey(pemKey.Bytes)
		if err != nil {
			// If we get here, then invalid key type
			return errors.New("manager: Unable to parse private key algorithm from disk")
		}
		// If we get here, then it's ECDSA
		algDisk = "ecdsa"
		sizeDisk = privKey.Curve.Params().BitSize
	} else {
		//If we get here, then it's RSA
		algDisk = "rsa"
		sizeDisk = privKey.N.BitLen()
	}

	// Check algorithm and keysize of private key on disk against what's defined in spec
	algSpec := csrRequest.KeyRequest.Algo()
	sizeSpec := csrRequest.KeyRequest.Size()

	if algDisk != algSpec {
		metrics.AlgorithmMismatchCount.WithLabelValues(spec.Path).Set(1)
		return fmt.Errorf("manager: disk alg is %s but spec alg is %s", algDisk, algSpec)
	}
	metrics.AlgorithmMismatchCount.WithLabelValues(spec.Path).Set(0)

	if sizeDisk != sizeSpec {
		metrics.KeysizeMismatchCount.WithLabelValues(spec.Path).Set(1)
		return fmt.Errorf("manager: disk key size is %d but spec key size is %d", sizeDisk, sizeSpec)
	}
	metrics.KeysizeMismatchCount.WithLabelValues(spec.Path).Set(0)

	// Check that certificate hostnames match spec hostnames
	certData, err := ioutil.ReadFile(certPath)
	if err != nil {
		return err
	}
	p, _ := pem.Decode(certData)
	if p == nil {
		return errors.New("Unable to pem decode certificate on disk")
	}
	cert, err := x509.ParseCertificate(p.Bytes)
	if err != nil {
		return err
	}
	if !hostnamesEquals(csrRequest.Hosts, cert.DNSNames) {
		metrics.HostnameMismatchCount.WithLabelValues(spec.Path).Set(1)
		return errors.New("manager: DNS names in cert on disk don't match with hostnames in spec")
	}
	metrics.HostnameMismatchCount.WithLabelValues(spec.Path).Set(0)

	// Check if cert and key are valid pair
	tlsCert, err := tls.X509KeyPair(certData, keyData)
	if err != nil || tlsCert.Leaf != nil {
		metrics.KeypairMismatchCount.WithLabelValues(spec.Path).Set(1)
		return fmt.Errorf("manager: Certificate and key on disk are not valid keypair: %s", err)
	}
	metrics.KeypairMismatchCount.WithLabelValues(spec.Path).Set(0)
	return nil
}

// CertExpireTime returns the time at which this spec's Certificate is no
// longer valid.
func (spec *Spec) CertExpireTime() time.Time {
	cert := spec.tr.Provider.Certificate()
	if cert != nil {
		return spec.tr.Provider.Certificate().NotAfter
	}
	return time.Time{}
}

// CAExpireTime returns the time at which this spec's CA is no
// longer valid.
func (spec *Spec) CAExpireTime() time.Time {
	c := spec.CA.pem
	if c == nil {
		log.Debug("spec %s: No CA loaded", spec)
		return time.Time{}
	}
	certPem, _ := pem.Decode(c)
	if certPem == nil {
		log.Debug("spec %s: Unable to pem decode CA certificate", spec)
		return time.Time{}
	}
	parsedCert, err := x509.ParseCertificate(certPem.Bytes)
	if err != nil {
		log.Debug("spec %s: Unable to parse certificate", spec)
		return time.Time{}
	}
	return parsedCert.NotAfter
}

// ResetLifespan Reset the lifespan to force cfssl to regenerate
func (spec *Spec) ResetLifespan() {
	cert := spec.tr.Provider.Certificate()
	if cert != nil {
		spec.tr.Provider.Certificate().NotAfter = time.Time{}
	}
}

// Certificate returns the x509.Certificate associated with the spec
// if one exists.
func (spec *Spec) Certificate() *x509.Certificate {
	if spec.tr == nil {
		panic("cert: cannot retrieve certificate because spec has an invalid transport")
	}

	return spec.tr.Provider.Certificate()
}

// Backoff returns the backoff delay.
func (spec *Spec) Backoff() time.Duration {
	return spec.tr.Backoff.Duration()
}

// ResetBackoff resets the spec's backoff.
func (spec *Spec) ResetBackoff() {
	spec.tr.Backoff.Reset()
}

// EnforcePKI Process a spec updating content on disk, taking action as needed.
// Returns (TTL for PKI, error).  If an error occurs, the ttl is at best
// a hint to the invoker as to when the next refresh is required- that said
// the invoker should back off and try a refresh.
func (spec *Spec) EnforcePKI(enableActions bool) (time.Duration, error) {
	err := spec.CheckDiskPKI()
	if err != nil {
		log.Debugf("manager: %s, checkdiskpki: %s.  Forcing refresh.", spec, err.Error())
		spec.ResetLifespan()
	}

	if err = spec.CheckCA(); err != nil {
		log.Errorf("manager: the CA for %s has changed, but the service couldn't be notified of the change", spec)
	}

	lifespan := time.Duration(0)
	if !spec.Ready() {
		log.Debugf("manager: %s isn't ready", spec)
	} else {
		log.Debugf("manager: %s checking lifespan", spec)
		lifespan = spec.Lifespan()
	}
	log.Debugf("manager: %s has lifespan %s", spec, lifespan)
	if lifespan <= 0 {
		err := spec.RenewPKI()
		if err != nil {
			log.Errorf("manager: failed to renew %s; requeuing cert", spec)
			return 0, err
		}

		log.Debug("taking action due to key refresh")
		if enableActions {
			err = spec.TakeAction("key")
		} else {
			log.Infof("skipping actions for %s due to calling mode", spec)
		}

		// Even though there was an error managing the service
		// associated with the certificate, the certificate has been
		// renewed.
		if err != nil {
			metrics.ActionFailure.WithLabelValues(spec.Path, "key").Inc()
			log.Errorf("manager: %s", err)
		}

		log.Info("manager: certificate successfully processed")
	}
	metrics.Expires.WithLabelValues(spec.Path, "cert").Set(float64(spec.CertExpireTime().Unix()))

	return spec.Lifespan(), nil
}

// TakeAction execute the configured svcmgr Action for this spec
func (spec *Spec) TakeAction(changeType string) error {
	log.Infof("manager: executing configured action due to change type %s for %s", changeType, spec.Cert.Path)
	caPath := ""
	if spec.CA.File != nil {
		caPath = spec.CA.File.Path
	}
	metrics.ActionCount.WithLabelValues(spec.Cert.Path, changeType).Inc()
	return spec.serviceManager.TakeAction(changeType, spec.Path, caPath, spec.Cert.Path, spec.Key.Path)
}

// The maximum number of attempts before giving up.
const maxAttempts = 5

// RenewPKI Try to update the on disk PKI content with a fresh CA/cert as needed
func (spec *Spec) RenewPKI() error {
	start := time.Now()
	for attempts := 0; attempts < maxAttempts; attempts++ {
		log.Infof("manager: processing certificate %s (attempt %d)", spec, attempts+1)
		err := spec.RefreshKeys()
		if err != nil {
			if isAuthError(err) {
				// Killing the server is really the
				// only valid option here; it will
				// force an investigation into why the
				// auth key is bad.
				log.Fatalf("invalid auth key for %s", spec)
			}
			backoff := spec.Backoff()
			log.Warningf("manager: failed to renew certificate (err=%s), backing off for %0.0f seconds", err, backoff.Seconds())
			metrics.FailureCount.WithLabelValues(spec.Path).Inc()
			time.Sleep(backoff)
			continue
		}

		spec.ResetBackoff()
		return nil
	}
	stop := time.Now()

	spec.ResetBackoff()
	return fmt.Errorf("manager: failed to renew %s in %d attempts (in %0.0f seconds)", spec, maxAttempts, stop.Sub(start).Seconds())
}

// CheckCA checks the CA on the certificate and restarts the service
// if needed.
func (spec *Spec) CheckCA() error {
	var err error
	var changed bool
	if changed, err = spec.CA.Refresh(); err != nil {
		metrics.ActionFailure.WithLabelValues(spec.Path, "CA").Inc()
		return err
	} else if changed {
		metrics.Expires.WithLabelValues(spec.Path, "ca").Set(float64(spec.CAExpireTime().Unix()))
		log.Debug("taking action due to CA refresh")
		err := spec.TakeAction("CA")

		if err != nil {
			metrics.ActionFailure.WithLabelValues(spec.Path, "CA").Inc()
			log.Errorf("manager: %s", err)
		}
	}
	metrics.Expires.WithLabelValues(spec.Path, "ca").Set(float64(spec.CAExpireTime().Unix()))
	return err
}
