package kubernetes

import (
	"errors"
	"fmt"
	"html/template"
	"strings"

	log "github.com/Sirupsen/logrus"

	"github.com/zalando-incubator/mate/pkg"
	"github.com/zalando-incubator/mate/pkg/kubernetes"
	k8s "k8s.io/client-go/kubernetes"
	api "k8s.io/client-go/pkg/api/v1"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/pkg/watch"
)

type kubernetesIngressProducer struct {
	client  *k8s.Clientset
	tmpl    *template.Template
	channel chan *pkg.Endpoint
}

func NewKubernetesIngress() (*kubernetesIngressProducer, error) {
	if params.domain == "" {
		return nil, errors.New("Please provide --kubernetes-domain")
	}

	client, err := kubernetes.NewClient(params.kubeServer)
	if err != nil {
		return nil, fmt.Errorf("Unable to setup Kubernetes API client: %v", err)
	}

	tmpl, err := template.New("endpoint").Funcs(template.FuncMap{
		"trimPrefix": strings.TrimPrefix,
	}).Parse(params.format)
	if err != nil {
		return nil, fmt.Errorf("Error parsing template: %s", err)
	}

	return &kubernetesIngressProducer{
		client:  client,
		tmpl:    tmpl,
		channel: make(chan *pkg.Endpoint),
	}, nil
}

func (a *kubernetesIngressProducer) Endpoints() ([]*pkg.Endpoint, error) {
	allIngress, err := a.client.Ingresses(api.NamespaceAll).List(api.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("Unable to retrieve list of ingress: %v", err)
	}

	endpoints := make([]*pkg.Endpoint, 0)

	for _, ing := range allIngress.Items {
		if err := validateIngress(ing); err != nil {
			log.Warnln(err)
			continue
		}

		eps, err := a.convertIngressToEndpoint(ing)
		if err != nil {
			// TODO: consider allowing the service continue running and just log this error
			return nil, err
		}

		endpoints = append(endpoints, eps...)
	}

	return endpoints, nil
}

func (a *kubernetesIngressProducer) StartWatch() error {
	w, err := a.client.Ingresses(api.NamespaceAll).Watch(api.ListOptions{})
	if err != nil {
		return fmt.Errorf("Unable to watch list of ingress: %v", err)
	}

	for event := range w.ResultChan() {
		if event.Type == watch.Error {
			// TODO: consider allowing the service continue running and just log this error
			return fmt.Errorf("Event listener received an error, terminating: %v", event)
		}

		if event.Type != watch.Added && event.Type != watch.Modified {
			continue
		}

		ing, ok := event.Object.(*extensions.Ingress)
		if !ok {
			// If the object wasn't a Service we can safely ignore it
			log.Printf("Cannot cast object to ingress: %v", ing)
			continue
		}

		log.Printf("%s: %s/%s", event.Type, ing.Namespace, ing.Name)

		if err := validateIngress(*ing); err != nil {
			log.Warnln(err)
			continue
		}

		eps, err := a.convertIngressToEndpoint(*ing)
		if err != nil {
			// TODO: consider letting the service continue running and just log this error
			return err
		}

		for _, ep := range eps {
			a.channel <- ep
		}
	}

	return pkg.ErrEventChannelClosed
}

func (a *kubernetesIngressProducer) ResultChan() (chan *pkg.Endpoint, error) {
	return a.channel, nil
}

func validateIngress(ing extensions.Ingress) error {
	switch {
	case len(ing.Status.LoadBalancer.Ingress) == 0:
		return fmt.Errorf(
			"The load balancer of ingress '%s/%s' does not have any ingress.",
			ing.Namespace, ing.Name,
		)
	case len(ing.Status.LoadBalancer.Ingress) > 1:
		// TODO(linki): in case we have multiple ingress we can just create multiple A or CNAME records
		return fmt.Errorf(
			"Cannot register ingress '%s/%s'. More than one ingress is not supported",
			ing.Namespace, ing.Name,
		)
	}

	return nil
}

func (a *kubernetesIngressProducer) convertIngressToEndpoint(ing extensions.Ingress) ([]*pkg.Endpoint, error) {
	endpoints := make([]*pkg.Endpoint, 0, len(ing.Spec.Rules))

	for _, rule := range ing.Spec.Rules {
		ep := &pkg.Endpoint{}

		for _, i := range ing.Status.LoadBalancer.Ingress {
			ep.IP = i.IP
			ep.Hostname = i.Hostname

			// take the first entry and exit
			// TODO(linki): we could easily return a list of endpoints
			break
		}

		ep.DNSName = pkg.SanitizeDNSName(rule.Host)

		endpoints = append(endpoints, ep)
	}

	return endpoints, nil
}
