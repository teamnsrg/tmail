// Provide tools for DKIM support

package core

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

// DkimConfig represents DKIM configuration for a domain
type DkimConfig struct {
	Id       int64
	Domain   string
	PubKey   string `sql:"type:text;"`
	PrivKey  string `sql:"type:text;"`
	Selector string
	Headers  string
}

func DkimAddExisting(domain string, pubkey_b64 string, privkey_b64 string) (error) {
    d := DkimConfig{
		Domain   : domain,
		PubKey   : pubkey_b64,
		PrivKey  : privkey_b64,
	    Selector : strconv.FormatInt(time.Now().Unix(), 10), //This is what is expected.
		Headers  : "",
	}
	return DB.Save(d).Error
}

// DkimEnable enabled DKIM on domain
func DkimEnable(domain string) (dkc *DkimConfig, err error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	// Check if DKIM is alreadu enabled
	dkc = &DkimConfig{}
	err = DB.Where("domain = ?", domain).Find(dkc).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	} else if err == nil {
		return nil, errors.New("DKIM is already enabled on " + domain)
	}

	// Create new key pairs
	privKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, err
	}
	privKeyBlock := pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   x509.MarshalPKCS1PrivateKey(privKey),
	}
	// privKeyPem
	privKeyPem := string(pem.EncodeToMemory(&privKeyBlock))
	pubKeyDer, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		return nil, err
	}
	pubKeyBlock := pem.Block{
		Type:    "PUBLIC KEY",
		Headers: nil,
		Bytes:   pubKeyDer,
	}
	t := strings.Split(string(pem.EncodeToMemory(&pubKeyBlock)), "\n")
	pubKey := strings.Join(t[1:len(t)-2], "")

	// selector: unique to prevent collision with existing record
	selector := strconv.FormatInt(time.Now().Unix(), 10)

	// save
	dkc = &DkimConfig{
		Domain:   domain,
		PubKey:   pubKey,
		PrivKey:  privKeyPem,
		Selector: selector,
		Headers:  "",
	}

	err = DB.Save(dkc).Error
	return dkc, err
}

// DkimDisable Disable DKIM for domain domain by removing his
// DkimConfig entry
func DkimDisable(domain string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	// Check if DKIM is alreadu enabled
	err := DB.Where("domain = ?", domain).Delete(&DkimConfig{}).Error
	if err != nil && err == gorm.ErrRecordNotFound {
		return nil
	}
	return err
}

// DkimGetConfig returns DKIM config for domain domain
func DkimGetConfig(domain string) (dkc *DkimConfig, err error) {
	dkc = &DkimConfig{}
	domain = strings.ToLower(domain)
	err = DB.Where("domain = ?", domain).First(dkc).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		} else {
			return nil, err
		}
	}
	return dkc, nil
}
