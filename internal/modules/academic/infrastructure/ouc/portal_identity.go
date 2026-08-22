package ouc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

var errPortalStudentMismatch = errors.New("OUC portal student number mismatch")

type portalIdentity struct {
	RealName  string
	StudentNo string
	Identity  string
}

type portalIdentityResponse struct {
	Code *int `json:"e"`
	Data *struct {
		Info *struct {
			RealName  string `json:"name"`
			StudentNo string `json:"xgh"`
			Identity  string `json:"identity"`
		} `json:"info"`
	} `json:"d"`
}

func parsePortalIdentity(body []byte, expectedStudentNo string) (portalIdentity, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var response portalIdentityResponse
	if err := decoder.Decode(&response); err != nil {
		return portalIdentity{}, fmt.Errorf("decode OUC portal identity: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return portalIdentity{}, fmt.Errorf("decode OUC portal identity: %w", err)
	}
	if response.Code == nil || *response.Code != 0 ||
		response.Data == nil || response.Data.Info == nil {
		return portalIdentity{}, errors.New("OUC portal did not return a successful identity")
	}

	identity := portalIdentity{
		RealName:  strings.TrimSpace(response.Data.Info.RealName),
		StudentNo: strings.TrimSpace(response.Data.Info.StudentNo),
		Identity:  strings.TrimSpace(response.Data.Info.Identity),
	}
	if identity.RealName == "" || utf8.RuneCountInString(identity.RealName) > 128 ||
		identity.StudentNo == "" || len(identity.StudentNo) > 64 ||
		identity.Identity == "" || utf8.RuneCountInString(identity.Identity) > 64 {
		return portalIdentity{}, errors.New("OUC portal returned an incomplete identity")
	}
	if identity.StudentNo != strings.TrimSpace(expectedStudentNo) {
		return portalIdentity{}, errPortalStudentMismatch
	}
	return identity, nil
}
