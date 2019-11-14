package mfa

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/aws-okta/lib/client/types"

	log "github.com/sirupsen/logrus"

	u2fhost "github.com/marshallbrekka/go-u2fhost"
)

const (
	MaxOpenRetries = 10
	RetryDelayMS   = 200 * time.Millisecond
)

var (
	errNoDeviceFound = fmt.Errorf("no U2F devices found. device might not be plugged in")
)

// FIDODevice is implementation of MFADevice for SMS
type FIDODevice struct {
	userInput     Input
	codeRequested bool
}

// Supported will check if the mfa config can be used by this device
func (d *FIDODevice) Supported(factor types.OktaUserAuthnFactor) error {
	if factor.FactorType == "u2f" && factor.Provider == "FIDO" {
		return nil
	}
	return fmt.Errorf("FIDODevice doesn't support %s %w", factor.FactorType, types.ErrNotSupported)
}

// Verify is called to get generate the payload that will be sent to Okta.
//   We will call this twice, once to tell Okta to send the code then
//   Once to prompt the user using `CodeSupplier` for the code.
func (d *FIDODevice) Verify(authResp types.OktaUserAuthn) (string, []byte, error) {
	var code string

	if d.codeRequested {

		f := authResp.Embedded.Factor
		fidoClient, err := NewFidoClient(f.Embedded.Challenge.Nonce,
			f.Profile.AppId,
			f.Profile.Version,
			f.Profile.CredentialId,
			authResp.StateToken)
		if err != nil {
			return "", []byte{}, err
		}
		signedAssertion, err := fidoClient.ChallengeU2f()
		if err != nil {
			return "", []byte{}, err
		}
		// re-assign the payload to provide U2F responses.
		payload, err := json.Marshal(signedAssertion)
		if err != nil {
			return "", []byte{}, err
		}
		return "verify", payload, nil
	} else {
		d.codeRequested = true
	}
	payload, err := json.Marshal(basicPayload{
		StateToken: authResp.StateToken,
		PassCode:   code,
	})

	return "verify", payload, err
}

type FidoClient struct {
	ChallengeNonce string
	AppId          string
	Version        string
	Device         u2fhost.Device
	KeyHandle      string
	StateToken     string
}

type SignedAssertion struct {
	StateToken    string `json:"stateToken"`
	ClientData    string `json:"clientData"`
	SignatureData string `json:"signatureData"`
}

func NewFidoClient(challengeNonce, appId, version, keyHandle, stateToken string) (FidoClient, error) {
	var device u2fhost.Device
	var err error

	retryCount := 0
	for retryCount < MaxOpenRetries {
		device, err = findDevice()
		if err != nil {
			if err == errNoDeviceFound {
				return FidoClient{}, err
			}

			retryCount++
			time.Sleep(RetryDelayMS)
			continue
		}

		return FidoClient{
			Device:         device,
			ChallengeNonce: challengeNonce,
			AppId:          appId,
			Version:        version,
			KeyHandle:      keyHandle,
			StateToken:     stateToken,
		}, nil
	}

	return FidoClient{}, fmt.Errorf("failed to create client: %s. exceeded max retries of %d", err, MaxOpenRetries)
}

func (d *FidoClient) ChallengeU2f() (*SignedAssertion, error) {

	if d.Device == nil {
		return nil, errors.New("No Device Found")
	}
	request := &u2fhost.AuthenticateRequest{
		Challenge: d.ChallengeNonce,
		// the appid is the only facet.
		Facet:     d.AppId,
		AppId:     d.AppId,
		KeyHandle: d.KeyHandle,
	}
	// do the change
	prompted := false
	timeout := time.After(time.Second * 25)
	interval := time.NewTicker(time.Millisecond * 250)
	var responsePayload *SignedAssertion

	d.Device.Open()

	defer func() {
		d.Device.Close()
	}()
	defer interval.Stop()
	for {
		select {
		case <-timeout:
			return nil, errors.New("Failed to get authentication response after 25 seconds")
		case <-interval.C:
			response, err := d.Device.Authenticate(request)
			if err == nil {
				responsePayload = &SignedAssertion{
					StateToken:    d.StateToken,
					ClientData:    response.ClientData,
					SignatureData: response.SignatureData,
				}
				fmt.Printf("  ==> Touch accepted. Proceeding with authentication\n")
				return responsePayload, nil
			}

			switch t := err.(type) {
			case *u2fhost.TestOfUserPresenceRequiredError:
				if !prompted {
					fmt.Printf("\nTouch the flashing U2F device to authenticate...\n")
					prompted = true
				}
			default:
				log.Debug("Got ErrType: ", t)
				return responsePayload, err
			}
		}
	}

	return responsePayload, nil
}

func findDevice() (u2fhost.Device, error) {
	var err error

	allDevices := u2fhost.Devices()
	if len(allDevices) == 0 {
		return nil, errNoDeviceFound
	}

	for i, device := range allDevices {
		err = device.Open()
		if err != nil {
			log.Debugf("failed to open device: %s", err)
			device.Close()

			continue
		}

		return allDevices[i], nil
	}

	return nil, fmt.Errorf("failed to open fido U2F device: %s", err)
}
