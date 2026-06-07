package auth

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	authv1 "github.com/dio/transit/examples/orange/api/orange/auth/v1"
)

// PolicyMap maps Connect procedure path (e.g. "/orange.egress.v1.EgressService/Heartbeat")
// to the auth options declared on that method via (orange.auth.v1.auth).
// Methods without the annotation are absent from the map (public).
type PolicyMap map[string]*authv1.AuthOptions

// BuildPolicyMap walks all globally registered proto files once and extracts
// every (orange.auth.v1.auth) annotation. Called once at interceptor creation.
// This is shared by all interceptors that use proto-based auth annotations.
func BuildPolicyMap() PolicyMap {
	m := make(PolicyMap)
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := range svcs.Len() {
			svc := svcs.Get(i)
			methods := svc.Methods()
			for j := range methods.Len() {
				method := methods.Get(j)
				if method.Options() == nil {
					continue
				}
				opts, _ := proto.GetExtension(method.Options(), authv1.E_Auth).(*authv1.AuthOptions)
				if opts == nil {
					continue
				}
				procedure := "/" + string(svc.FullName()) + "/" + string(method.Name())
				m[procedure] = opts
			}
		}
		return true
	})
	return m
}
