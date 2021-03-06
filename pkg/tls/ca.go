package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"
)

type (
	// CA provides a certificate authority for TLS-enabled installs.
	// Issuing certificates concurrently is not supported.
	CA struct {
		// Cred contains the CA's credentials.
		Cred Cred

		// Validity configures the NotBefore and NotAfter parameters for certificates
		// issued by this CA.
		//
		// Currently this is used for the CA's validity too, but nothing should
		// assume that the CA's validity period is the same as issued certificates'
		// validity.
		Validity Validity

		// nextSerialNumber is the serial number of the next certificate to issue.
		// Serial numbers must not be reused.
		//
		// It is assumed there is only one instance of CA and it is assumed that a
		// given CA object isn't requested to issue certificates concurrently.
		//
		// For now we do not attempt to meet CABForum requirements (e.g. regarding
		// randomness).
		nextSerialNumber uint64
	}

	// Validity configures the expiry times of issued certificates.
	Validity struct {
		// Validity is the duration for which issued certificates are valid. This
		// is approximately cert.NotAfter - cert.NotBefore with some additional
		// allowance for clock skew.
		//
		// Currently this is used for the CA's validity too, but nothing should
		// assume that the CA's validity period is the same as issued certificates'
		// validity.
		Lifetime time.Duration

		// ClockSkewAllowance is the maximum supported clock skew. Everything that
		// processes the certificates must have a system clock that is off by no
		// more than this allowance in either direction.
		ClockSkewAllowance time.Duration
	}
)

const (
	// DefaultLifetime configures certificate validity.
	//
	// Initially all certificates will be valid for one year.
	//
	// TODO: Shorten the validity duration of CA and end-entity certificates downward.
	DefaultLifetime = (24 * 365) * time.Hour

	// DefaultClockSkewAllowance indicates the maximum allowed difference in clocks
	// in the network.
	//
	// Allow an two hours of clock skew.
	//
	// TODO: decrease the default value of this and make it tunable.
	//
	// TODO: Reconsider how this interacts with the similar logic in the webpki
	// verifier; since both are trying to account for clock skew, there is
	// somewhat of an over-correction.
	DefaultClockSkewAllowance = 2 * time.Hour
)

// NewCA initializes a new CA with default settings.
func NewCA(cred Cred, validity Validity) *CA {
	return &CA{cred, validity, uint64(1)}
}

// CreateRootCA configures a new root CA with the given settings
func CreateRootCA(
	name string,
	key *ecdsa.PrivateKey,
	validity Validity,
) (*CA, error) {
	// Configure the root certificate.
	t := createTemplate(1, &key.PublicKey, validity)
	t.Subject = pkix.Name{CommonName: name}
	t.IsCA = true
	t.MaxPathLen = -1
	t.BasicConstraintsValid = true
	t.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign

	// Self-sign the root certificate.
	crtb, err := x509.CreateCertificate(rand.Reader, t, t, key.Public(), key)
	if err != nil {
		return nil, err
	}
	c, err := x509.ParseCertificate(crtb)
	if err != nil {
		return nil, err
	}

	// The Crt has an empty TrustChain because it's at the root.
	cred := validCredOrPanic(key, Crt{Certificate: c})
	ca := NewCA(cred, validity)
	ca.nextSerialNumber++ // Because we've already created the root cert.
	return ca, nil
}

// GenerateKey creates a new P-256 ECDSA private key from the default random
// source.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// GenerateRootCAWithDefaults generates a new root CA with default settings.
func GenerateRootCAWithDefaults(name string) (*CA, error) {
	// Generate a new root key.
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}

	return CreateRootCA(name, key, Validity{})
}

// GenerateCA generates a new intermdiary CA.
func (ca *CA) GenerateCA(name string, validity Validity, maxPathLen int) (*CA, error) {
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}

	t := ca.createTemplate(&key.PublicKey)
	t.Subject = pkix.Name{CommonName: name}
	t.IsCA = true
	t.MaxPathLen = maxPathLen
	t.MaxPathLenZero = true // 0-values are actually 0
	t.BasicConstraintsValid = true
	t.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	crt, err := ca.Cred.SignCrt(t)
	if err != nil {
		return nil, err
	}

	return NewCA(validCredOrPanic(key, *crt), ca.Validity), nil
}

// GenerateEndEntityCred creates a new certificate that is valid for the
// given DNS name, generating a new keypair for it.
func (ca *CA) GenerateEndEntityCred(dnsName string) (*Cred, error) {
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}

	csr := x509.CertificateRequest{
		Subject:   pkix.Name{CommonName: dnsName},
		DNSNames:  []string{dnsName},
		PublicKey: &key.PublicKey,
	}
	crt, err := ca.SignEndEntityCrt(&csr)
	if err != nil {
		return nil, err
	}

	c := validCredOrPanic(key, *crt)
	return &c, nil
}

// SignEndEntityCrt creates a new certificate that is valid for the
// given DNS name, generating a new keypair for it.
func (ca *CA) SignEndEntityCrt(csr *x509.CertificateRequest) (*Crt, error) {
	pubkey, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("CSR must contain an ECDSA public key: %+v", csr.PublicKey)
	}

	t := ca.createTemplate(pubkey)
	t.Issuer = ca.Cred.Crt.Certificate.Subject
	t.Subject = csr.Subject
	t.Extensions = csr.Extensions
	t.ExtraExtensions = csr.ExtraExtensions
	t.DNSNames = csr.DNSNames
	t.EmailAddresses = csr.EmailAddresses
	t.IPAddresses = csr.IPAddresses
	t.URIs = csr.URIs

	return ca.Cred.SignCrt(t)
}

// createTemplate returns a certificate t for a non-CA certificate with
// no subject name, no subjectAltNames. The t can then be modified into
// a (root) CA t or an end-entity t by the caller.
func (ca *CA) createTemplate(pubkey *ecdsa.PublicKey) *x509.Certificate {
	c := createTemplate(ca.nextSerialNumber, pubkey, ca.Validity)
	ca.nextSerialNumber++
	return c
}

// createTemplate returns a certificate t for a non-CA certificate with
// no subject name, no subjectAltNames. The t can then be modified into
// a (root) CA t or an end-entity t by the caller.
func createTemplate(
	serialNumber uint64,
	k *ecdsa.PublicKey,
	v Validity,
) *x509.Certificate {
	// ECDSA is used instead of RSA because ECDSA key generation is
	// straightforward and fast whereas RSA key generation is extremely slow
	// and error-prone.
	//
	// CA certificates are signed with the same algorithm as end-entity
	// certificates because they are relatively short-lived, because using one
	// algorithm minimizes exposure to implementation flaws, and to speed up
	// signature verification time.
	//
	// SHA-256 is used because any larger digest would be truncated to 256 bits
	// anyway since a P-256 scalar is only 256 bits long.
	const SignatureAlgorithm = x509.ECDSAWithSHA256

	notBefore, notAfter := v.Window(time.Now())

	return &x509.Certificate{
		SerialNumber:       big.NewInt(int64(serialNumber)),
		SignatureAlgorithm: SignatureAlgorithm,
		NotBefore:          notBefore,
		NotAfter:           notAfter,
		PublicKey:          k,
		KeyUsage:           x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
	}
}

// Window returns the time window for which a certificate should be valid.
func (v *Validity) Window(t time.Time) (time.Time, time.Time) {
	life := v.Lifetime
	if life == 0 {
		life = DefaultLifetime
	}
	skew := v.ClockSkewAllowance
	if skew == 0 {
		skew = DefaultClockSkewAllowance
	}
	start := t.Add(-skew)
	end := t.Add(life).Add(skew)
	return start, end
}
