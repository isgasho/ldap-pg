package main

import (
	"fmt"
	"strings"

	goldap "gopkg.in/ldap.v3"
)

type DN struct {
	DNNorm          string
	DNOrig          string
	ReverseParentDN string
	cachedRDN       map[string]string
}

func normalizeDN(dn string) (*DN, error) {
	d, err := parseDN(dn)
	if err != nil {
		return nil, err
	}

	reverse := toReverseDN(d)

	return &DN{
		DNNorm:          strings.Join(d, ","),
		ReverseParentDN: reverse,
		DNOrig:          dn,
	}, nil
}

func toReverseDN(dn []string) string {
	var path string
	// ignore last rdn
	for i := len(dn) - 1; i > 0; i-- {
		path += strings.ToLower(dn[i]) + "/"
	}
	return path
}

func (d *DN) Equal(o *DN) bool {
	return d.DNNorm == o.DNNorm
}

func (d *DN) GetRDN() map[string]string {
	if len(d.cachedRDN) > 0 {
		return d.cachedRDN
	}
	dn, _ := goldap.ParseDN(d.DNNorm)

	m := make(map[string]string, len(dn.RDNs[0].Attributes))

	for _, a := range dn.RDNs[0].Attributes {
		m[a.Type] = a.Value
	}

	d.cachedRDN = m

	return m
}

func (d *DN) Modify(newRDN string) (*DN, error) {
	nd, err := goldap.ParseDN(newRDN)
	if err != nil {
		return nil, err
	}

	dn, _ := goldap.ParseDN(d.DNOrig)

	var n []string

	for _, v := range nd.RDNs {
		for _, a := range v.Attributes {
			n = append(n, fmt.Sprintf("%s=%s", a.Type, a.Value))
			// TODO multiple RDN using +
		}
	}

	for i := 1; i < len(dn.RDNs); i++ {
		for _, a := range dn.RDNs[i].Attributes {
			n = append(n, fmt.Sprintf("%s=%s", a.Type, a.Value))
			// TODO multiple RDN using +
		}
	}

	newDNOrig := strings.Join(n, ",")

	return normalizeDN(newDNOrig)
}

func (d *DN) ToPath() string {
	parts := strings.Split(d.DNNorm, ",")

	var path string
	for i := len(parts) - 1; i >= 0; i-- {
		path += strings.ToLower(parts[i]) + "/"
	}
	return path
}