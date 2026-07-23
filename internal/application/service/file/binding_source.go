package file

import (
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	storageBindingSourceDirect = "direct"
	storageBindingSourceTenant = "tenant"
	storageBindingSourceGlobal = "global"
)

func setStorageBindingIdentity(service interfaces.FileService, source, credentialScope string) interfaces.FileService {
	setCredentialProfile := func(provider storagebinding.ProviderName) string {
		ref, err := storagebinding.CredentialProfileReference(
			storagebinding.CredentialScope(credentialScope), provider, "default",
		)
		if err != nil {
			return ""
		}
		return ref
	}
	switch svc := service.(type) {
	case *localFileService:
		svc.bindingSource = source
	case *DummyFileService:
		svc.bindingSource = source
	case *minioFileService:
		svc.bindingSource = source
		svc.credentialScope = credentialScope
		svc.credentialRef = setCredentialProfile(storagebinding.ProviderMinIO)
	case *s3FileService:
		svc.bindingSource = source
		svc.credentialScope = credentialScope
		svc.credentialRef = setCredentialProfile(storagebinding.ProviderS3)
	case *cosFileService:
		svc.bindingSource = source
		svc.credentialScope = credentialScope
		svc.credentialRef = setCredentialProfile(storagebinding.ProviderCOS)
	case *tosFileService:
		svc.bindingSource = source
		svc.credentialScope = credentialScope
		svc.credentialRef = setCredentialProfile(storagebinding.ProviderTOS)
	case *ossFileService:
		svc.bindingSource = source
		svc.credentialScope = credentialScope
		svc.credentialRef = setCredentialProfile(storagebinding.ProviderOSS)
	case *ks3FileService:
		svc.bindingSource = source
		svc.credentialScope = credentialScope
		svc.credentialRef = setCredentialProfile(storagebinding.ProviderKS3)
	case *obsFileService:
		svc.bindingSource = source
		svc.credentialScope = credentialScope
		svc.credentialRef = setCredentialProfile(storagebinding.ProviderOBS)
	}
	return service
}

// MarkGlobalStorageService records that a process-wide service is backed by
// the stable global/provider credential profile. Public constructors default
// to direct injection because they cannot otherwise know their owner.
func MarkGlobalStorageService(service interfaces.FileService) interfaces.FileService {
	return setStorageBindingIdentity(service, storageBindingSourceGlobal, storageBindingSourceGlobal)
}
