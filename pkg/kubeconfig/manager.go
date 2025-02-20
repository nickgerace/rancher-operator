package kubeconfig

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"

	v1 "github.com/rancher/rancher-operator/pkg/apis/rancher.cattle.io/v1"
	"github.com/rancher/rancher-operator/pkg/clients"
	mgmtcontrollers "github.com/rancher/rancher-operator/pkg/generated/controllers/management.cattle.io/v3"
	"github.com/rancher/rancher-operator/pkg/settings"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	appcontroller "github.com/rancher/wrangler/pkg/generated/controllers/apps/v1"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/pkg/randomtoken"
	corev1 "k8s.io/api/core/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	userIDLabel     = "authn.management.cattle.io/token-userId"
	tokenKindLabel  = "authn.management.cattle.io/kind"
	tokenHashedAnno = "authn.management.cattle.io/token-hashed"
	systemNamespace = "cattle-system"

	hashFormat = "$%d:%s:%s" // $version:salt:hash -> $1:abc:def
	Version    = 2
)

type Manager struct {
	deploymentCache appcontroller.DeploymentCache
	daemonsetCache  appcontroller.DaemonSetCache
	tokens          mgmtcontrollers.TokenClient
	userCache       mgmtcontrollers.UserCache
	users           mgmtcontrollers.UserClient
	secretCache     corecontrollers.SecretCache
	secrets         corecontrollers.SecretClient
	settings        mgmtcontrollers.SettingCache
}

func New(clients *clients.Clients) *Manager {
	return &Manager{
		deploymentCache: clients.Apps.Deployment().Cache(),
		daemonsetCache:  clients.Apps.DaemonSet().Cache(),
		tokens:          clients.Management.Token(),
		userCache:       clients.Management.User().Cache(),
		users:           clients.Management.User(),
		secretCache:     clients.Core.Secret().Cache(),
		secrets:         clients.Core.Secret(),
		settings:        clients.Management.Setting().Cache(),
	}
}

func GetKubeConfigSecretName(clusterName string) string {
	return clusterName + "-kubeconfig"
}

func (m *Manager) GetToken(clusterNamespace, clusterName string) (string, error) {
	kubeConfigSecretName := GetKubeConfigSecretName(clusterName)
	if token, err := m.getSavedToken(clusterNamespace, kubeConfigSecretName); err != nil || token != "" {
		return token, err
	}

	// Need to be careful about caches being out of sync since we are dealing with multiple objects that
	// arent eventually consistent (because we delete and create the token for the user)
	if token, err := m.getSavedTokenNoCache(clusterNamespace, kubeConfigSecretName); err != nil || token != "" {
		return token, err
	}

	userName, err := m.EnsureUser(clusterNamespace, clusterName)
	if err != nil {
		return "", err
	}

	return m.createUserToken(userName)
}

func (m *Manager) EnsureUser(clusterNamespace, clusterName string) (string, error) {
	principalID := getPrincipalID(clusterNamespace, clusterName)
	userName := getUserNameForPrincipal(principalID)
	return userName, m.createUser(principalID, userName)
}

func getUserNameForPrincipal(principal string) string {
	hasher := sha256.New()
	hasher.Write([]byte(principal))
	sha := base32.StdEncoding.WithPadding(-1).EncodeToString(hasher.Sum(nil))[:10]
	return "u-" + strings.ToLower(sha)
}

func labelsForUser(principalID string) map[string]string {
	encodedPrincipalID := base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(principalID))
	if len(encodedPrincipalID) > 63 {
		encodedPrincipalID = encodedPrincipalID[:63]
	}
	return map[string]string{
		encodedPrincipalID: "hashed-principal-name",
	}
}

func (m *Manager) getSavedToken(kubeConfigNamespace, kubeConfigName string) (string, error) {
	secret, err := m.secretCache.Get(kubeConfigNamespace, kubeConfigName)
	if apierror.IsNotFound(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return string(secret.Data["token"]), nil
}

func (m *Manager) getSavedTokenNoCache(kubeConfigNamespace, kubeConfigName string) (string, error) {
	secret, err := m.secrets.Get(kubeConfigNamespace, kubeConfigName, metav1.GetOptions{})
	if apierror.IsNotFound(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return string(secret.Data["token"]), nil
}

func getPrincipalID(clusterNamespace, clusterName string) string {
	return fmt.Sprintf("system://provisioning/%s/%s", clusterNamespace, clusterName)
}

func (m *Manager) createUser(principalID, userName string) error {
	_, err := m.userCache.Get(userName)
	if apierror.IsNotFound(err) {
		_, err = m.users.Create(&v3.User{
			ObjectMeta: metav1.ObjectMeta{
				Name:   userName,
				Labels: labelsForUser(principalID),
			},
			PrincipalIDs: []string{
				principalID,
			},
		})
	}
	return err
}

func (m *Manager) createUserToken(userName string) (string, error) {
	_, err := m.tokens.Get(userName, metav1.GetOptions{})
	if err == nil {
		err = m.tokens.Delete(userName, nil)
	}
	if err != nil && !apierror.IsNotFound(err) {
		return "", err
	}

	tokenValue, err := randomtoken.Generate()
	if err != nil {
		return "", fmt.Errorf("failed to generate token key: %w", err)
	}

	token := &v3.Token{
		ObjectMeta: metav1.ObjectMeta{
			Name: userName,
			Labels: map[string]string{
				userIDLabel:    userName,
				tokenKindLabel: "provisioning",
			},
			Annotations: map[string]string{},
		},
		UserID:       userName,
		AuthProvider: "local",
		IsDerived:    true,
		Token:        tokenValue,
	}

	if ok, err := settings.Bool(m.settings, "token-hashing"); err != nil {
		return "", err
	} else if ok {
		tokenHash, err := createSHA256Hash(tokenValue)
		if err != nil {
			return "", err
		}
		token.Token = tokenHash
		token.Annotations[tokenHashedAnno] = "true"
	}

	_, err = m.tokens.Create(token)
	return fmt.Sprintf("%s:%s", userName, tokenValue), err
}

func createSHA256Hash(secretKey string) (string, error) {
	salt := make([]byte, 8)
	_, err := rand.Read(salt)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s%s", salt, secretKey)))
	encSalt := base64.RawStdEncoding.EncodeToString(salt)
	encKey := base64.RawStdEncoding.EncodeToString(hash[:])
	return fmt.Sprintf(hashFormat, Version, encSalt, encKey), nil
}

func (m *Manager) GetKubeConfig(cluster *v1.Cluster, status v1.ClusterStatus) (*corev1.Secret, error) {
	var (
		name       = GetKubeConfigSecretName(cluster.Name)
		tokenValue string
	)

	if cluster.Spec.ImportedConfig != nil && cluster.Spec.ImportedConfig.KubeConfigSecretName == name {
		return nil, nil
	}

	tokenValue, err := m.GetToken(cluster.Namespace, cluster.Name)
	if err != nil {
		return nil, err
	}

	serverURL, cacert, err := m.GetServerURLAndCA()
	if err != nil {
		return nil, err
	}

	data, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"cluster": {
				Server:                   fmt.Sprintf("%s/k8s/clusters/%s", serverURL, status.ClusterName),
				CertificateAuthorityData: []byte(strings.TrimSpace(cacert)),
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"user": {
				Token: tokenValue,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"default": {
				Cluster:  "cluster",
				AuthInfo: "user",
			},
		},
		CurrentContext: "default",
	})
	if err != nil {
		return nil, err
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      name,
		},
		Data: map[string][]byte{
			"value": data,
			"token": []byte(tokenValue),
		},
	}, nil
}

func (m *Manager) GetServerURLAndCA() (string, string, error) {
	serverURL, ca, err := settings.GetServerURLAndCA(m.settings)
	if err != nil {
		return "", "", err
	}

	tlsSecret, err := m.secretCache.Get(systemNamespace, "tls-rancher-internal-ca")
	if err != nil {
		return "", "", err
	}
	internalCA := string(tlsSecret.Data[corev1.TLSCertKey])

	if dp, err := m.deploymentCache.Get(systemNamespace, "rancher"); err == nil && dp.Spec.Replicas != nil && *dp.Spec.Replicas != 0 {
		return fmt.Sprintf("https://rancher.%s", systemNamespace), internalCA, nil
	}

	if _, err := m.daemonsetCache.Get(systemNamespace, "rancher"); err == nil {
		return fmt.Sprintf("https://rancher.%s", systemNamespace), internalCA, nil
	}

	return serverURL, ca, nil
}
