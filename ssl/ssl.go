package ssl

import (
	"bytes"
	"fmt"
	"html/template"
	glog "log"
	"os"
	"time"

	"github.com/SUSE/scf-secret-generator/model"
	"github.com/SUSE/scf-secret-generator/util"
	"github.com/cloudflare/cfssl/cli/genkey"
	"github.com/cloudflare/cfssl/config"
	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/helpers"
	"github.com/cloudflare/cfssl/initca"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"

	"k8s.io/api/core/v1"
)

const defaultCA = "cacert"

// CertInfo contains all the information required to generate an SSL cert
type CertInfo struct {
	PrivateKeyName  string // Name to associate with private key
	CertificateName string // Name to associate with certificate
	IsAuthority     bool

	SubjectNames []string
	RoleName     string

	Certificate []byte
	PrivateKey  []byte
}

// RecordCertInfo record cert information for later generation
func RecordCertInfo(certInfo map[string]CertInfo, configVar *model.ConfigurationVariable) {
	info := certInfo[configVar.Generator.ID]

	switch configVar.Generator.ValueType {
	case model.ValueTypeCertificate:
		info.CertificateName = util.ConvertNameToKey(configVar.Name)
	case model.ValueTypePrivateKey:
		info.PrivateKeyName = util.ConvertNameToKey(configVar.Name)
	default:
		glog.Printf("Invalid certificate generator value_type: %s", configVar.Generator.ValueType)
	}
	info.IsAuthority = (configVar.Generator.Type == model.GeneratorTypeCACertificate)

	if len(configVar.Generator.SubjectNames) > 0 {
		info.SubjectNames = configVar.Generator.SubjectNames
	}
	if configVar.Generator.RoleName != "" {
		info.RoleName = configVar.Generator.RoleName
	}
	certInfo[configVar.Generator.ID] = info
}

// GenerateCerts creates an SSL cert and private key
func GenerateCerts(certInfo map[string]CertInfo, namespace, domain, serviceDomainSuffix string, secrets *v1.Secret) error {
	// generate all the CAs first because they are needed to sign the certs
	for id, info := range certInfo {
		if !info.IsAuthority {
			continue
		}
		glog.Printf("- SSL CA: %s\n", id)
		err := createCA(certInfo, secrets, id)
		if err != nil {
			return err
		}
	}
	for id, info := range certInfo {
		if info.IsAuthority {
			continue
		}
		glog.Printf("- SSL CRT: %s (%s / %s)\n", id, info.CertificateName, info.PrivateKeyName)
		if len(info.SubjectNames) == 0 && info.RoleName == "" {
			fmt.Fprintf(os.Stderr, "Warning: certificate %s has no names\n", info.CertificateName)
		}
		err := createCert(certInfo, namespace, domain, serviceDomainSuffix, secrets, id)
		if err != nil {
			return err
		}
	}
	return nil
}

func rsaKeyRequest() *csr.BasicKeyRequest {
	return &csr.BasicKeyRequest{A: "rsa", S: 4096}
}

func createCA(certInfo map[string]CertInfo, secrets *v1.Secret, id string) error {
	var err error
	info := certInfo[id]

	if len(secrets.Data[info.PrivateKeyName]) > 0 {
		// fetch CA from secrets because we may need it to sign new certs
		info.PrivateKey = secrets.Data[info.PrivateKeyName]
		info.Certificate = secrets.Data[info.CertificateName]
		certInfo[id] = info
		return nil
	}

	req := &csr.CertificateRequest{
		CA:         &csr.CAConfig{Expiry: "262800h"}, // 30 years
		CN:         "SCF CA",
		KeyRequest: rsaKeyRequest(),
	}
	info.Certificate, _, info.PrivateKey, err = initca.New(req)
	if err != nil {
		return fmt.Errorf("Cannot create CA: %s", err)
	}

	secrets.Data[info.PrivateKeyName] = info.PrivateKey
	secrets.Data[info.CertificateName] = info.Certificate

	certInfo[id] = info
	return nil
}

func addHost(req *csr.CertificateRequest, wildcard bool, name string) {
	req.Hosts = append(req.Hosts, name)
	if wildcard {
		req.Hosts = append(req.Hosts, "*."+name)
	}
}

func createCert(certInfo map[string]CertInfo, namespace, domain, serviceDomainSuffix string, secrets *v1.Secret, id string) error {
	var err error
	info := certInfo[id]

	if len(secrets.Data[info.PrivateKeyName]) > 0 {
		return nil
	}

	// XXX Add support for multiple CAs
	caInfo := certInfo[defaultCA]
	if len(caInfo.PrivateKey) == 0 || len(caInfo.Certificate) == 0 {
		return fmt.Errorf("CA %s not found", defaultCA)
	}

	req := &csr.CertificateRequest{KeyRequest: rsaKeyRequest()}

	if info.RoleName != "" {
		addHost(req, true, info.RoleName)
		addHost(req, true, fmt.Sprintf("%s.%s.svc", info.RoleName, namespace))
		addHost(req, true, fmt.Sprintf("%s.%s.svc.cluster.local", info.RoleName, namespace))

		// Generate wildcard certs for stateful sets for self-clustering roles
		// We do this instead of having a bunch of subject alt names so that the
		// certs can work correctly if we scale the cluster post-deployment.
		prefix := fmt.Sprintf("*.%s-set", info.RoleName)
		addHost(req, false, prefix)
		addHost(req, false, fmt.Sprintf("%s.%s.svc", prefix, namespace))
		addHost(req, false, fmt.Sprintf("%s.%s.svc.cluster.local", prefix, namespace))

		addHost(req, true, fmt.Sprintf("%s.%s", info.RoleName, serviceDomainSuffix))
	}

	for _, name := range info.SubjectNames {
		t, err := template.New("").Parse(name)
		if err != nil {
			return fmt.Errorf("Can't parse subject name '%s' for certificate '%s': %s", name, id, err)
		}
		buf := &bytes.Buffer{}
		mapping := map[string]string{
			"DOMAIN":                     domain,
			"KUBERNETES_NAMESPACE":       namespace,
			"KUBE_SERVICE_DOMAIN_SUFFIX": serviceDomainSuffix,
		}
		err = t.Execute(buf, mapping)
		if err != nil {
			return err
		}
		addHost(req, false, buf.String())
	}

	if len(req.Hosts) == 0 {
		req.Hosts = append(req.Hosts, info.CertificateName)
	}
	req.CN = req.Hosts[0]

	var signingReq []byte
	g := &csr.Generator{Validator: genkey.Validator}
	signingReq, info.PrivateKey, err = g.ProcessRequest(req)
	if err != nil {
		return fmt.Errorf("Cannot generate cert: %s", err)
	}

	caCert, err := helpers.ParseCertificatePEM(caInfo.Certificate)
	if err != nil {
		return fmt.Errorf("Cannot parse CA cert: %s", err)
	}
	caKey, err := helpers.ParsePrivateKeyPEM(caInfo.PrivateKey)
	if err != nil {
		return fmt.Errorf("Cannot parse CA private key: %s", err)
	}

	signingProfile := &config.SigningProfile{
		Usage:        []string{"server auth", "client auth"},
		Expiry:       262800 * time.Hour, // 30 years
		ExpiryString: "262800h",          // 30 years
	}
	policy := &config.Signing{
		Profiles: map[string]*config.SigningProfile{},
		Default:  signingProfile,
	}

	s, err := local.NewSigner(caKey, caCert, signer.DefaultSigAlgo(caKey), policy)
	if err != nil {
		return fmt.Errorf("Cannot create signer: %s", err)
	}

	info.Certificate, err = s.Sign(signer.SignRequest{Request: string(signingReq)})
	if err != nil {
		return fmt.Errorf("Failed to sign cert: %s", err)
	}

	if len(info.PrivateKeyName) == 0 {
		return fmt.Errorf("Certificate %s created with empty private key name", id)
	}
	if len(info.PrivateKey) == 0 {
		return fmt.Errorf("Certificate %s created with empty private key", id)
	}
	if len(info.CertificateName) == 0 {
		return fmt.Errorf("Certificate %s created with empty certificate name", id)
	}
	if len(info.Certificate) == 0 {
		return fmt.Errorf("Certificate %s created with empty certificate", id)
	}
	secrets.Data[info.PrivateKeyName] = info.PrivateKey
	secrets.Data[info.CertificateName] = info.Certificate
	certInfo[id] = info

	return nil
}
