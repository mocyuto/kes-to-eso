package parser

import (
	"context"
	"errors"
	"fmt"
	"kestoeso/pkg/apis"
	"kestoeso/pkg/provider"
	"kestoeso/pkg/utils"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	api "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	//	"k8s.io/client-go/util/homedir"
	//	"k8s.io/client-go/kubernetes"
	//	"k8s.io/client-go/rest"
	//	"k8s.io/client-go/tools/clientcmd"

	yaml "sigs.k8s.io/yaml"
)

// Store DB Functions

type SecretStoreDB []api.SecretStore
type StoreDB interface {
	Exists(S api.SecretStore) (bool, int)
}

func (storedb SecretStoreDB) Exists(S api.SecretStore) (bool, int) {
	for idx, secretStore := range storedb {
		if S.Kind == "SecretStore" &&
			secretStore.Namespace == S.Namespace &&
			secretStore.APIVersion == S.APIVersion &&
			secretStore.Kind == S.Kind &&
			reflect.DeepEqual(secretStore.Spec, S.Spec) {
			return true, idx
		} else if S.Kind == "ClusterSecretStore" &&
			secretStore.APIVersion == S.APIVersion &&
			secretStore.Kind == S.Kind &&
			reflect.DeepEqual(secretStore.Spec, S.Spec) {
			return true, idx
		}
	}
	return false, -1
}

var ESOSecretStoreList = make(SecretStoreDB, 0)

func readKESFromFile(file string) (apis.KESExternalSecret, error) {
	dat, err := os.ReadFile(file)
	if err != nil {
		return apis.KESExternalSecret{}, err
	}
	var K = apis.KESExternalSecret{}
	err = yaml.Unmarshal(dat, &K)
	if err != nil {
		return apis.KESExternalSecret{}, err
	}
	return K, nil
}

func NewESOSecret() api.ExternalSecret {
	d := api.ExternalSecret{}
	d.TypeMeta = metav1.TypeMeta{
		Kind:       "ExternalSecret",
		APIVersion: "external-secrets.io/v1beta1",
	}
	return d
}

func mapLoop(m map[string]interface{}) error {
	for k := range m {
		if k != "metadata" && k != "type" && k != "data" {
			return fmt.Errorf("%v templating is currently not supported", k)
		}
	}
	return nil
}

func canMigrateKes(K apis.KESExternalSecret) error {
	err := mapLoop(K.Spec.Template)
	if err != nil {
		return err
	}
	for _, data := range K.Spec.Data {
		if data.Path != "" {
			return errors.New("externalSecret with path selection is currently not supported")
		}
	}
	return nil
}

func getBackendType(K apis.KESExternalSecret) string {
	//	var spec apis.KESExternalSecretSpec
	if K.Spec.BackendType != "" {
		return K.Spec.BackendType
	}

	if K.SecretDescriptor.BackendType != "" {
		return K.SecretDescriptor.BackendType
	}

	return ""
}

func bindProvider(ctx context.Context, S api.SecretStore, K apis.KESExternalSecret, client *provider.KesToEsoClient) (api.SecretStore, bool) {
	if client.Options.TargetNamespace != "" {
		S.ObjectMeta.Namespace = client.Options.TargetNamespace
	} else {
		S.ObjectMeta.Namespace = K.ObjectMeta.Namespace
	}
	var err error
	backend := getBackendType(K)
	switch backend {
	case "secretsManager":
		if client.Options.AWSOptions.AuthType == "" {
			log.Error("AWS Auth Type must be set")
			return S, false
		}
		p := api.AWSProvider{Service: api.AWSServiceSecretsManager, Region: K.Spec.Region}
		prov := api.SecretStoreProvider{}
		prov.AWS = &p
		S.Spec.Provider = &prov
		S, err = client.InstallAWSSecrets(ctx, K, S)
		if err != nil {
			log.Warnf("Failed to Install AWS Backend Specific configuration: %v. Make sure you have set up Controller Pod Identity or manually edit SecretStore before applying it", err)
		}
	case "systemManager":
		if client.Options.AWSOptions.AuthType == "" {
			log.Error("AWS Auth Type must be set")
			return S, false
		}
		p := api.AWSProvider{Service: api.AWSServiceParameterStore, Region: K.Spec.Region}
		prov := api.SecretStoreProvider{}
		prov.AWS = &p
		S.Spec.Provider = &prov
		S, err = client.InstallAWSSecrets(ctx, K, S)
		if err != nil {
			log.Warnf("Failed to Install AWS Backend Specific configuration: %v. Make sure you have set up Controller Pod Identity Manually Edit SecretStore before applying it", err)
		}
	case "azureKeyVault": // TODO RECHECK MAPPING ON REAL USE CASE. WHAT KEYVAULTNAME IS USED FOR?
		p := api.AzureKVProvider{}
		prov := api.SecretStoreProvider{}
		prov.AzureKV = &p
		S.Spec.Provider = &prov
		vaultUrl := fmt.Sprintf("https://%v.vault.azure.net", K.Spec.KeyVaultName)
		S.Spec.Provider.AzureKV.VaultURL = &vaultUrl
		S, err = client.InstallAzureKVSecrets(ctx, S)
		if err != nil {
			log.Warnf("Failed to Install Azure Backend Specific configuration: %v. Manually Edit SecretStore before applying it", err)
		}
	case "gcpSecretsManager":
		p := api.GCPSMProvider{}
		p.ProjectID = K.Spec.ProjectID
		prov := api.SecretStoreProvider{}
		prov.GCPSM = &p
		S.Spec.Provider = &prov
		S, err = client.InstallGCPSMSecrets(ctx, S)
		if err != nil {
			log.Warnf("Failed to Install GCP Backend Specific configuration: %v. Makesure you have set up workload identity or manually edit SecretStore before applying it", err)
		}
	case "ibmcloudSecretsManager":
		prov := api.SecretStoreProvider{}
		prov.IBM = &api.IBMProvider{}
		S.Spec.Provider = &prov
		S, err = client.InstallIBMSecrets(ctx, S)
		if err != nil {
			log.Warnf("Failed to Install IBM Backend Specific configuration: %v. Manually Edit SecretStore before applying it", err)
		}
	case "vault": // TODO RECHECK MAPPING ON REAL USE CASE
		p := api.VaultProvider{}
		if K.Spec.KvVersion == 1 {
			p.Version = api.VaultKVStoreV1
		} else {
			p.Version = api.VaultKVStoreV2
			prefix := ""
			for _, data := range K.Spec.Data {
				if prefix == "" {
					pref := strings.Split(data.Key, "/")[0]
					prefix = pref
				}
				if prefix != strings.Split(data.Key, "/")[0] {
					log.Fatal("Failed to parse secret store for KES secret!")
					return S, false
				}
			}
			p.Path = &prefix
		}
		prov := api.SecretStoreProvider{}
		prov.Vault = &p
		S.Spec.Provider = &prov
		S, err = client.InstallVaultSecrets(ctx, S)
		if err != nil {
			log.Warnf("Failed to Install Vault Backend Specific configuration: %v. Manually Edit SecretStore before applying it", err)
			kubeauth := api.VaultKubernetesAuth{}
			S.Spec.Provider.Vault.Auth.Kubernetes = &kubeauth
		}
		if K.Spec.VaultMountPoint != "" {
			S.Spec.Provider.Vault.Auth.Kubernetes.Path = K.Spec.VaultMountPoint
		}
		if K.Spec.VaultRole != "" {
			S.Spec.Provider.Vault.Auth.Kubernetes.Role = K.Spec.VaultRole
		}
	default:
		log.Warnf("Provider %v is not currently supported!", backend)
	}
	exists, pos := ESOSecretStoreList.Exists(S)
	if !exists {
		S.ObjectMeta.Name = fmt.Sprintf("%v-secretstore-%v-%v", strings.ToLower(backend), S.ObjectMeta.Namespace, K.ObjectMeta.Name)
		ESOSecretStoreList = append(ESOSecretStoreList, S)
		return S, true
	} else {
		return ESOSecretStoreList[pos], false
	}
}

func parseSpecifics(K apis.KESExternalSecret, E api.ExternalSecret) (api.ExternalSecret, error) {
	backend := K.Spec.BackendType
	ans := E
	switch backend {
	case "vault":
		if K.Spec.KvVersion == 2 {
			for idx, data := range ans.Spec.Data {
				paths := strings.Split(data.RemoteRef.Key, "/")
				if paths[1] != "data" { // we have the good format like <vaultname>/data/<path>/<to>/<secret>
					return E, errors.New("secret key not compatible with kv2 format (<vault>/data/<path>/<to>/<secret>)")
				}
				str := strings.Join(paths[2:], "/")
				ans.Spec.Data[idx].RemoteRef.Key = str
			}
		}
		for idx, data := range ans.Spec.Data {
			if data.RemoteRef.Property == "" {
				ans.Spec.Data[idx].RemoteRef.Property = ans.Spec.Data[idx].SecretKey
			}
		}
		for idx, dataFrom := range ans.Spec.DataFrom {
			paths := strings.Split(dataFrom.Extract.Key, "/")
			if paths[1] != "data" { // we have the good format like <vaultname>/data/<path>/<to>/<secret>
				return E, errors.New("secret key not compatible with kv2 format (<vault>/data/<path>/<to>/<secret>)")
			}
			str := strings.Join(paths[2:], "/")
			ans.Spec.DataFrom[idx].Extract.Key = str

		}
	default:
	}
	return ans, nil
}

func parseGenerals(K apis.KESExternalSecret, E api.ExternalSecret, options *apis.KesToEsoOptions) (api.ExternalSecret, error) {
	secret := E
	secret.ObjectMeta.Name = K.ObjectMeta.Name
	secret.Spec.Target.Name = K.ObjectMeta.Name // Inherits default in KES, so we should do the same approach here
	if options.TargetNamespace != "" {
		secret.ObjectMeta.Namespace = options.TargetNamespace
	} else {
		secret.ObjectMeta.Namespace = K.ObjectMeta.Namespace
	}

	for _, kesSecretData := range K.Spec.Data {
		applyData(kesSecretData, &secret)
	}
	for _, kesSecretDataFrom := range K.Spec.DataFrom {
		applyDataFrom(kesSecretDataFrom, &secret)
	}
	for _, data := range K.SecretDescriptor.Data {
		applyData(data, &secret)
	}
	for _, dataFrom := range K.SecretDescriptor.DataFrom {
		applyDataFrom(dataFrom, &secret)
	}
	templ, err := fillTemplate(secret.Spec.Target.Template, K.Spec.Template)
	if err != nil {
		return secret, err
	}
	secret.Spec.Target.Template = &templ
	return secret, nil
}

func applyData(data apis.KESExternalSecretData, E *api.ExternalSecret) {
	var refKey string
	if data.SecretType != "" {
		refKey = data.SecretType + "/" + data.Key
	} else {
		refKey = data.Key
	}
	esoRemoteRef := api.ExternalSecretDataRemoteRef{
		Key:      refKey,
		Property: data.Property,
		Version:  data.Version}
	esoSecretData := api.ExternalSecretData{
		SecretKey: data.Name,
		RemoteRef: esoRemoteRef}
	E.Spec.Data = append(E.Spec.Data, esoSecretData)
}
func applyDataFrom(dataFrom string, E *api.ExternalSecret) {
	esoDataFrom := api.ExternalSecretDataFromRemoteRef{
		Extract: &api.ExternalSecretDataRemoteRef{
			Key: dataFrom,
		},
	}
	E.Spec.DataFrom = append(E.Spec.DataFrom, esoDataFrom)
}

func fillTemplate(template *api.ExternalSecretTemplate, m map[string]interface{}) (api.ExternalSecretTemplate, error) {
	tm := api.ExternalSecretTemplateMetadata{}
	ans := api.ExternalSecretTemplate{}
	if template != nil {
		ans = *template
	}
	v, ok := m["type"]
	if ok {
		ans.Type = corev1.SecretType(v.(string))
	}
	v, ok = m["data"]
	if ok {
		ans.Data = make(map[string]string)
		metadata, ok := v.(map[string]interface{})
		if ok {
			for k, v := range metadata {
				ans.Data[k] = v.(string)
			}
		}
	}
	v, ok = m["metadata"]
	if ok {
		n, ok := v.(map[string]interface{})
		if ok {
			annot, okann := n["annotations"]
			if okann {
				tm.Annotations = make(map[string]string)
				meta, ok := annot.(map[string]interface{})
				if ok {
					for k, v := range meta {
						tm.Annotations[k] = v.(string)
					}
				}
			}
			label, oklab := n["labels"]
			if oklab {
				tm.Labels = make(map[string]string)
				meta, ok := label.(map[string]interface{})
				if ok {
					for k, v := range meta {
						tm.Labels[k] = v.(string)
					}
				}
			}
		}
	}
	ans.Metadata = api.ExternalSecretTemplateMetadata{}
	ans.Metadata = tm
	return ans, nil
}
func linkSecretStore(E api.ExternalSecret, S api.SecretStore) api.ExternalSecret {
	ext := E
	ext.Spec.SecretStoreRef.Name = S.ObjectMeta.Name
	ext.Spec.SecretStoreRef.Kind = S.TypeMeta.Kind
	return ext
}

type RootResponse struct {
	Path string
	Kes  apis.KESExternalSecret
	Es   api.ExternalSecret
	Ss   api.SecretStore
}

func Root(ctx context.Context, client *provider.KesToEsoClient) []RootResponse {
	ans := make([]RootResponse, 0)
	var files []string
	err := filepath.Walk(client.Options.InputPath, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, file := range files {
		log.Debugln("Looking for ", file)
		K, err := readKESFromFile(file)
		if err != nil {
			panic(err)
		}
		if !utils.IsKES(K) {
			log.Errorf("Not a KES File: %v\n", file)
			continue
		}
		err = canMigrateKes(K)
		if err != nil {
			log.Errorf("Cannot process file %v, %v. Skipping", file, err)
			continue
		}
		E, err := parseGenerals(K, NewESOSecret(), client.Options)
		if err != nil {
			log.Errorf("Could not process file %v: %v. Skipping.", file, err)
			continue
		}
		E, err = parseSpecifics(K, E)
		if err != nil {
			log.Errorf("Could not process file %v: %v. Skipping.", file, err)
			continue
		}
		S := utils.NewSecretStore(client.Options.SecretStore)
		S, newProvider := bindProvider(ctx, S, K, client)
		secretFilename := fmt.Sprintf("%v/external-secret-%v.yaml", client.Options.OutputPath, E.ObjectMeta.Name)
		if newProvider {
			storeFilename := fmt.Sprintf("%v/secret-store-%v.yaml", client.Options.OutputPath, S.ObjectMeta.Name)
			err = utils.WriteYaml(S, storeFilename, client.Options.ToStdout)
			if err != nil {
				panic(err)
			}
		}
		E = linkSecretStore(E, S)
		err = utils.WriteYaml(E, secretFilename, client.Options.ToStdout)
		if err != nil {
			panic(err)
		}
		response := RootResponse{
			Path: file,
			Kes:  K,
			Es:   E,
			Ss:   S,
		}
		ans = append(ans, response)
	}
	return ans
}

// Functions for kubernetes application management
