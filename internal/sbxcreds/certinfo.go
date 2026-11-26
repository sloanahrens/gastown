package sbxcreds

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"time"

	"gopkg.in/yaml.v3"
)

// kubeconfigUsers is the part of a kubeconfig certInfo reads.
type kubeconfigUsers struct {
	Users []struct {
		User struct {
			ClientCertificateData string `yaml:"client-certificate-data"`
		} `yaml:"user"`
	} `yaml:"users"`
}

// certInfo returns the first user's certificate subject and not-after. Each
// refusal names the problem and echoes none of the data, which can hold the
// private half; a yaml error is dropped because it can quote the document.
func certInfo(kubeconfig []byte) (subject string, notAfter time.Time, err error) {
	var doc kubeconfigUsers
	if err := yaml.Unmarshal(kubeconfig, &doc); err != nil {
		return "", time.Time{}, errors.New("the data is not a kubeconfig")
	}
	if len(doc.Users) == 0 {
		return "", time.Time{}, errors.New("the kubeconfig names no user")
	}
	data := doc.Users[0].User.ClientCertificateData
	if data == "" {
		return "", time.Time{}, errors.New("the kubeconfig's first user has no client-certificate-data")
	}
	pemBytes, decodeErr := base64.StdEncoding.DecodeString(data)
	block, _ := pem.Decode(pemBytes)
	if decodeErr != nil || block == nil || block.Type != "CERTIFICATE" {
		return "", time.Time{}, errors.New("the first user's client-certificate-data is not a base64 CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", time.Time{}, errors.New("the first user's certificate does not parse")
	}
	return cert.Subject.String(), cert.NotAfter, nil
}
