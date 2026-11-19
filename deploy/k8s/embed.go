package k8s

import _ "embed"

// The admission policies and the update Job's RBAC are applied by hand, so a
// running binary cannot read them from a render and compare them with the live
// objects. Each copy below is the file's text at the commit the binary was built
// from, so a compare against it means "the live object equals the running
// build's file" (gt-aar839).

// UpdateAdmissionYAML is update-admission.yaml's text at the building commit,
// which gt town cluster drift compares with the live policy and its binding
// (gt-aar839).
//
//go:embed update-admission.yaml
var UpdateAdmissionYAML []byte

// BdGatewayServiceAdmissionYAML is bd-gateway-service-admission.yaml's text at
// the building commit, which gt town cluster drift compares with the live policy
// and its binding (gt-aar839).
//
//go:embed bd-gateway-service-admission.yaml
var BdGatewayServiceAdmissionYAML []byte

// UpdateRBACYAML is update-rbac.yaml's text at the building commit, which
// gt town cluster drift compares with the live Roles and RoleBindings
// (gt-aar839).
//
//go:embed update-rbac.yaml
var UpdateRBACYAML []byte
