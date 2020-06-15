package factory

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"github.com/VictoriaMetrics/operator/conf"
	monitoringv1 "github.com/VictoriaMetrics/operator/pkg/apis/monitoring/v1"
	victoriametricsv1beta1 "github.com/VictoriaMetrics/operator/pkg/apis/victoriametrics/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
)

func CreateOrUpdateConfigurationSecret(ctx context.Context, cr *victoriametricsv1beta1.VmAgent, rclient client.Client, c *conf.BaseOperatorConf) error {
	// If no service or pod monitor selectors are configured, the user wants to
	// manage configuration themselves. Do create an empty Secret if it doesn't
	// exist.
	l := log.WithValues("vmagent", cr.Name(), "namespace", cr.Namespace)

	if cr.Spec.ServiceMonitorSelector == nil && cr.Spec.PodMonitorSelector == nil {
		l.Info("neither ServiceMonitor not PodMonitor selector specified, leaving configuration unmanaged")

		s, err := makeEmptyConfigurationSecret(cr, c)
		if err != nil {
			return fmt.Errorf("generating empty config secret failed: %w", err)
		}
		err = rclient.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: s.Name}, &v1.Secret{})
		if errors.IsNotFound(err) {
			if err := rclient.Create(ctx, s); err != nil && !errors.IsAlreadyExists(err) {
				return fmt.Errorf("creating empty config file failed: %w", err)
			}
		}
		if !errors.IsNotFound(err) && err != nil {
			return err
		}

		return nil
	}

	smons, err := SelectServiceMonitors(ctx, cr, rclient)
	if err != nil {
		return fmt.Errorf("selecting ServiceMonitors failed: %w", err)
	}

	pmons, err := SelectPodMonitors(ctx, cr, rclient)
	if err != nil {
		return fmt.Errorf("selecting PodMonitors failed: %w", err)
	}

	SecretsInNS := &v1.SecretList{}
	err = rclient.List(ctx, SecretsInNS)
	if err != nil {
		return fmt.Errorf("cannot list secrets at vmagent namespace: %w", err)
	}

	basicAuthSecrets, err := loadBasicAuthSecrets(ctx, rclient, smons, cr.Spec.APIServerConfig, SecretsInNS)

	if err != nil {
		return fmt.Errorf("cannot load basic secrets for vmagent: %w", err)
	}

	bearerTokens, err := loadBearerTokensFromSecrets(ctx, rclient, smons)
	if err != nil {
		return fmt.Errorf("cannot load bearer tokens from secrets for vmagent: %w", err)
	}

	additionalScrapeConfigs, err := loadAdditionalScrapeConfigsSecret(cr.Spec.AdditionalScrapeConfigs, SecretsInNS)
	if err != nil {
		return fmt.Errorf("loading additional scrape configs from Secret failed: %w", err)
	}

	// Update secret based on the most recent configuration.
	generatedConfig, err := generateConfig(
		cr,
		smons,
		pmons,
		basicAuthSecrets,
		bearerTokens,
		additionalScrapeConfigs,
	)
	if err != nil {
		return fmt.Errorf("generating config for vmagent failed: %w", err)
	}

	s := makeConfigSecret(cr, c)
	s.ObjectMeta.Annotations = map[string]string{
		"generated": "true",
	}

	// Compress config to avoid 1mb secret limit for a while
	var buf bytes.Buffer
	if err = gzipConfig(&buf, generatedConfig); err != nil {
		return fmt.Errorf("cannot gzip config for vmagent: %w", err)
	}
	s.Data[configFilename] = buf.Bytes()

	curSecret := &v1.Secret{}
	err = rclient.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: s.Name}, curSecret)
	if errors.IsNotFound(err) {
		log.Info("creating new configuration secret for vmagent")
		return rclient.Create(ctx, s)
	}

	var (
		generatedConf             = s.Data[configFilename]
		curConfig, curConfigFound = curSecret.Data[configFilename]
	)
	if curConfigFound {
		if bytes.Equal(curConfig, generatedConf) {
			log.Info("updating VmAgent configuration secret skipped, no configuration change")
			return nil
		}
		log.Info("current VmAgent configuration has changed")
	} else {
		log.Info("no current VmAgent configuration secret found", "currentConfigFound", curConfigFound)
	}

	log.Info("updating VmAgent configuration secret")
	return rclient.Update(ctx, s)
}

func SelectServiceMonitors(ctx context.Context, cr *victoriametricsv1beta1.VmAgent, rclient client.Client) (map[string]*monitoringv1.ServiceMonitor, error) {

	// Selectors (<namespace>/<name>) might overlap. Deduplicate them along the keyFunc.
	res := make(map[string]*monitoringv1.ServiceMonitor)

	namespaces := []string{}

	//list namespaces matched by  nameselector
	//for each namespace apply list with  selector
	//combine result
	if cr.Spec.ServiceMonitorNamespaceSelector == nil {
		namespaces = append(namespaces, cr.Namespace)
	} else if cr.Spec.ServiceMonitorNamespaceSelector.MatchExpressions == nil && cr.Spec.ServiceMonitorNamespaceSelector.MatchLabels == nil {
		namespaces = nil
	} else {
		log.Info("namspace selector for serviceMonitors", "selector", cr.Spec.ServiceMonitorNamespaceSelector.String())
		nsSelector, err := metav1.LabelSelectorAsSelector(cr.Spec.ServiceMonitorNamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("cannot convert rulenamespace selector: %w", err)
		}
		namespaces, err = selectNamespaces(ctx, rclient, nsSelector)
		if err != nil {
			return nil, fmt.Errorf("cannot select namespaces for rule match: %w", err)
		}
	}

	// if namespaces isn't nil, then nameSpaceSelector is defined
	// but monitorSelector maybe be nil and we must set it to catch all value
	if namespaces != nil && cr.Spec.ServiceMonitorSelector == nil {
		cr.Spec.ServiceMonitorSelector = &metav1.LabelSelector{}
	}
	servMonSelector, err := metav1.LabelSelectorAsSelector(cr.Spec.ServiceMonitorSelector)
	if err != nil {
		return nil, fmt.Errorf("cannot convert ServiceMonitorSelector to labelSelector: %w", err)
	}

	servMonsCombined := []monitoringv1.ServiceMonitor{}

	//list all namespaces for rules with selector
	if namespaces == nil {
		log.Info("listing all namespaces for serviceMonitors", "vmagent", cr.Name())
		servMons := &monitoringv1.ServiceMonitorList{}
		err = rclient.List(ctx, servMons, &client.ListOptions{LabelSelector: servMonSelector})
		if err != nil {
			return nil, fmt.Errorf("cannot list rules from all namespaces: %w", err)
		}
		servMonsCombined = append(servMonsCombined, servMons.Items...)

	} else {
		for _, ns := range namespaces {
			log.Info("listing namespace for serviceMonitors", "ns", ns, "vmagent", cr.Name())
			listOpts := &client.ListOptions{Namespace: ns, LabelSelector: servMonSelector}
			servMons := &monitoringv1.ServiceMonitorList{}
			err = rclient.List(ctx, servMons, listOpts)
			if err != nil {
				return nil, fmt.Errorf("cannot list rules at namespace: %s, err: %w", ns, err)
			}
			servMonsCombined = append(servMonsCombined, servMons.Items...)

		}
	}

	for _, mon := range servMonsCombined {
		m := mon.DeepCopy()
		res[mon.Namespace+"/"+mon.Name] = m
	}

	// If denied by Prometheus spec, filter out all service monitors that access
	// the file system.
	if cr.Spec.ArbitraryFSAccessThroughSMs.Deny {
		for namespaceAndName, sm := range res {
			for _, endpoint := range sm.Spec.Endpoints {
				if err := testForArbitraryFSAccess(endpoint); err != nil {
					delete(res, namespaceAndName)
					log.Info("skipping servicemonitor",
						"error", err.Error(),
						"servicemonitor", namespaceAndName,
						"namespace", cr.Namespace,
						"vmagent", cr.Name(),
					)
				}
			}
		}
	}

	serviceMonitors := []string{}
	for k := range res {
		serviceMonitors = append(serviceMonitors, k)
	}
	log.Info("selected ServiceMonitors", "servicemonitors", strings.Join(serviceMonitors, ","), "namespace", cr.Namespace, "vmagent", cr.Name())

	return res, nil
}

func SelectPodMonitors(ctx context.Context, cr *victoriametricsv1beta1.VmAgent, rclient client.Client) (map[string]*monitoringv1.PodMonitor, error) {
	// Selectors might overlap. Deduplicate them along the keyFunc.
	res := make(map[string]*monitoringv1.PodMonitor)

	namespaces := []string{}

	// list namespaces matched by  namespaceSelector
	// for each namespace apply list with  selector
	// combine result

	if cr.Spec.PodMonitorNamespaceSelector == nil {
		namespaces = append(namespaces, cr.Namespace)
	} else if cr.Spec.PodMonitorNamespaceSelector.MatchExpressions == nil && cr.Spec.PodMonitorNamespaceSelector.MatchLabels == nil {
		namespaces = nil
	} else {
		log.Info("selector for podMonitor", "vmagent", cr.Name(), "selector", cr.Spec.PodMonitorNamespaceSelector.String())
		nsSelector, err := metav1.LabelSelectorAsSelector(cr.Spec.PodMonitorNamespaceSelector)
		if err != nil {
			return nil, fmt.Errorf("cannot convert ruleNameSpace to labelSelector: %w", err)
		}
		namespaces, err = selectNamespaces(ctx, rclient, nsSelector)
		if err != nil {
			return nil, fmt.Errorf("cannot select namespaces for rule match: %w", err)
		}
	}

	// if namespaces isn't nil, then nameSpaceSelector is defined
	//but monitorSelector maybe be nil and we have to set it to catch all value
	if namespaces != nil && cr.Spec.PodMonitorSelector == nil {
		cr.Spec.PodMonitorSelector = &metav1.LabelSelector{}
	}
	podMonSelector, err := metav1.LabelSelectorAsSelector(cr.Spec.PodMonitorSelector)
	if err != nil {
		return nil, fmt.Errorf("cannot convert podMonitorSelector to label selector: %w", err)
	}

	podMonsCombined := []monitoringv1.PodMonitor{}

	//list all namespaces for rules with selector
	if namespaces == nil {
		log.Info("listing all namespaces for rules")
		servMons := &monitoringv1.PodMonitorList{}
		err = rclient.List(ctx, servMons, &client.ListOptions{LabelSelector: podMonSelector})
		if err != nil {
			return nil, fmt.Errorf("cannot list pod monitors from all namespaces: %w", err)
		}
		podMonsCombined = append(podMonsCombined, servMons.Items...)

	} else {
		for _, ns := range namespaces {
			listOpts := &client.ListOptions{Namespace: ns, LabelSelector: podMonSelector}
			servMons := &monitoringv1.PodMonitorList{}
			err = rclient.List(ctx, servMons, listOpts)
			if err != nil {
				return nil, fmt.Errorf("cannot list podmonitors at namespace: %s, err: %w", ns, err)
			}
			podMonsCombined = append(podMonsCombined, servMons.Items...)

		}
	}

	log.Info("filtering namespaces to select PodMonitors from",
		"namespace", cr.Namespace, "vmagent", cr.Name())
	for _, podMon := range podMonsCombined {
		pm := podMon.DeepCopy()
		res[podMon.Namespace+"/"+podMon.Name] = pm
	}
	podMonitors := make([]string, 0)
	for key := range res {
		podMonitors = append(podMonitors, key)
	}

	log.Info("selected PodMonitors", "podmonitors", strings.Join(podMonitors, ","), "namespace", cr.Namespace, "vmagent", cr.Name())

	return res, nil
}

func loadBasicAuthSecrets(
	ctx context.Context,
	rclient client.Client,
	mons map[string]*monitoringv1.ServiceMonitor,
	apiserverConfig *monitoringv1.APIServerConfig,
	SecretsInPromNS *v1.SecretList,
) (map[string]BasicAuthCredentials, error) {

	secrets := map[string]BasicAuthCredentials{}
	nsSecretCache := make(map[string]*v1.Secret)
	for _, mon := range mons {
		for i, ep := range mon.Spec.Endpoints {
			if ep.BasicAuth != nil {
				credentials, err := loadBasicAuthSecretFromAPI(ctx, rclient, ep.BasicAuth, mon.Namespace, nsSecretCache)
				if err != nil {
					return nil, fmt.Errorf("could not generate basicAuth for servicemonitor %s. %w", mon.Name, err)
				}
				secrets[fmt.Sprintf("serviceMonitor/%s/%s/%d", mon.Namespace, mon.Name, i)] = credentials
			}

		}
	}

	// load apiserver basic auth secret
	if apiserverConfig != nil && apiserverConfig.BasicAuth != nil {
		credentials, err := loadBasicAuthSecret(apiserverConfig.BasicAuth, SecretsInPromNS)
		if err != nil {
			return nil, fmt.Errorf("could not generate basicAuth for apiserver config. %w", err)
		}
		secrets["apiserver"] = credentials
	}

	return secrets, nil

}

func loadBearerTokensFromSecrets(ctx context.Context, rclient client.Client, mons map[string]*monitoringv1.ServiceMonitor) (map[string]BearerToken, error) {
	tokens := map[string]BearerToken{}
	nsSecretCache := make(map[string]*v1.Secret)

	for _, mon := range mons {
		for i, ep := range mon.Spec.Endpoints {
			if ep.BearerTokenSecret.Name == "" {
				continue
			}

			token, err := getCredFromSecret(
				ctx,
				rclient,
				mon.Namespace,
				ep.BearerTokenSecret,
				mon.Namespace+"/"+ep.BearerTokenSecret.Name,
				nsSecretCache,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"failed to extract endpoint bearertoken for servicemonitor %v from secret %v in namespace %v",
					mon.Name, ep.BearerTokenSecret.Name, mon.Namespace,
				)
			}

			tokens[fmt.Sprintf("serviceMonitor/%s/%s/%d", mon.Namespace, mon.Name, i)] = BearerToken(token)
		}
	}

	return tokens, nil
}

func loadBasicAuthSecret(basicAuth *monitoringv1.BasicAuth, s *v1.SecretList) (BasicAuthCredentials, error) {
	var username string
	var password string
	var err error

	for _, secret := range s.Items {

		if secret.Name == basicAuth.Username.Name {
			if username, err = extractCredKey(&secret, basicAuth.Username); err != nil {
				return BasicAuthCredentials{}, err
			}
		}

		if secret.Name == basicAuth.Password.Name {
			if password, err = extractCredKey(&secret, basicAuth.Password); err != nil {
				return BasicAuthCredentials{}, err
			}

		}
		if username != "" && password != "" {
			break
		}
	}

	if username == "" && password == "" {
		return BasicAuthCredentials{}, fmt.Errorf("basic auth username and password secret not found")
	}

	return BasicAuthCredentials{username: username, password: password}, nil

}

func extractCredKey(secret *v1.Secret, sel v1.SecretKeySelector) (string, error) {
	if s, ok := secret.Data[sel.Key]; ok {
		return string(s), nil
	}
	return "", fmt.Errorf("secret key %q in secret %q not found", sel.Key, sel.Name)
}

func getCredFromSecret(
	ctx context.Context,
	rclient client.Client,
	ns string,
	sel v1.SecretKeySelector,
	cacheKey string,
	cache map[string]*v1.Secret,
) (string, error) {
	var s *v1.Secret
	var ok bool

	if s, ok = cache[cacheKey]; !ok {
		s = &v1.Secret{}
		if err := rclient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sel.Name}, s); err != nil {
			return "", fmt.Errorf("unable to fetch key from secret%s: %w", sel.Name, err)
		}
		cache[cacheKey] = s
	}
	return extractCredKey(s, sel)
}

func getCredFromConfigMap(
	ctx context.Context,
	rclient client.Client,
	ns string,
	sel v1.ConfigMapKeySelector,
	cacheKey string,
	cache map[string]*v1.ConfigMap,
) (string, error) {
	var s *v1.ConfigMap
	var ok bool

	if s, ok = cache[cacheKey]; !ok {
		s = &v1.ConfigMap{}
		err := rclient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sel.Name}, s)
		if err != nil {
			return "", fmt.Errorf("cannot get configmap: %s at namespace %s, err: %s", sel.Name, ns, err)
		}
		cache[cacheKey] = s
	}

	if a, ok := s.Data[sel.Key]; ok {
		return a, nil
	}
	return "", fmt.Errorf("key not found at configmap, key: %s, configmap %s ", sel.Key, sel.Name)
}

func loadBasicAuthSecretFromAPI(ctx context.Context, rclient client.Client, basicAuth *monitoringv1.BasicAuth, ns string, cache map[string]*v1.Secret) (BasicAuthCredentials, error) {
	var username string
	var password string
	var err error

	if username, err = getCredFromSecret(ctx, rclient, ns, basicAuth.Username, ns+"/"+basicAuth.Username.Name, cache); err != nil {
		return BasicAuthCredentials{}, err
	}

	if password, err = getCredFromSecret(ctx, rclient, ns, basicAuth.Password, ns+"/"+basicAuth.Password.Name, cache); err != nil {
		return BasicAuthCredentials{}, err
	}

	return BasicAuthCredentials{username: username, password: password}, nil
}

func loadAdditionalScrapeConfigsSecret(additionalScrapeConfigs *v1.SecretKeySelector, s *v1.SecretList) ([]byte, error) {
	if additionalScrapeConfigs != nil {
		for _, secret := range s.Items {
			if secret.Name == additionalScrapeConfigs.Name {
				if c, ok := secret.Data[additionalScrapeConfigs.Key]; ok {
					return c, nil
				}

				return nil, fmt.Errorf("key %v could not be found in Secret %v", additionalScrapeConfigs.Key, additionalScrapeConfigs.Name)
			}
		}
		if additionalScrapeConfigs.Optional == nil || !*additionalScrapeConfigs.Optional {
			return nil, fmt.Errorf("secret %v could not be found", additionalScrapeConfigs.Name)
		}
	}
	return nil, nil
}

func testForArbitraryFSAccess(e monitoringv1.Endpoint) error {
	if e.BearerTokenFile != "" {
		return fmt.Errorf("it accesses file system via bearer token file which VmAgent specification prohibits")
	}

	tlsConf := e.TLSConfig
	if tlsConf == nil {
		return nil
	}

	if err := e.TLSConfig.Validate(); err != nil {
		return err
	}

	if tlsConf.CAFile != "" || tlsConf.CertFile != "" || tlsConf.KeyFile != "" {
		return fmt.Errorf("it accesses file system via tls config which VmAgent specification prohibits")
	}

	return nil
}

func gzipConfig(buf *bytes.Buffer, conf []byte) error {
	w := gzip.NewWriter(buf)
	defer w.Close()
	if _, err := w.Write(conf); err != nil {
		return err
	}
	return nil
}
