package service

import "github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"

func syntheticLocalTestBinding() (storagebinding.Binding, error) {
	return storagebinding.Normalize(storagebinding.Binding{
		Provider:           storagebinding.ProviderLocal,
		CanonicalLocalBase: "/test/storage",
		LocalRootIdentity:  "test:volume:1",
		ConfigSource:       storagebinding.ConfigSourceDirect,
		CredentialScope:    storagebinding.CredentialScopeNone,
	})
}

func (s *cleanupFileServiceStub) BindingForPath(string) (storagebinding.Binding, error) {
	return syntheticLocalTestBinding()
}

func (s *createKnowledgeFileServiceStub) BindingForPath(string) (storagebinding.Binding, error) {
	return syntheticLocalTestBinding()
}
