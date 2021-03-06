package configsyncer

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/rancher/norman/controller"
	"github.com/rancher/rancher/pkg/controllers/user/alert/common"
	alertconfig "github.com/rancher/rancher/pkg/controllers/user/alert/config"
	"github.com/rancher/rancher/pkg/controllers/user/alert/deployer"
	"github.com/rancher/rancher/pkg/controllers/user/alert/manager"
	monitorutil "github.com/rancher/rancher/pkg/monitoring"
	"github.com/rancher/rancher/pkg/ref"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rancher/types/apis/core/v1"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/rancher/types/config"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var (
	defaultGroupInterval  = 10
	eventGroupInterval    = 1
	defaultGroupWait      = 10
	defaultRepeatInterval = 10
)

func NewConfigSyncer(ctx context.Context, cluster *config.UserContext, alertManager *manager.AlertManager, operatorCRDManager *manager.PromOperatorCRDManager) *ConfigSyncer {
	return &ConfigSyncer{
		secretsGetter:           cluster.Core,
		clusterAlertGroupLister: cluster.Management.Management.ClusterAlertGroups(cluster.ClusterName).Controller().Lister(),
		projectAlertGroupLister: cluster.Management.Management.ProjectAlertGroups("").Controller().Lister(),
		clusterAlertRuleLister:  cluster.Management.Management.ClusterAlertRules(cluster.ClusterName).Controller().Lister(),
		projectAlertRuleLister:  cluster.Management.Management.ProjectAlertRules("").Controller().Lister(),
		notifierLister:          cluster.Management.Management.Notifiers(cluster.ClusterName).Controller().Lister(),
		clusterName:             cluster.ClusterName,
		alertManager:            alertManager,
		operatorCRDManager:      operatorCRDManager,
	}
}

type ConfigSyncer struct {
	secretsGetter           v1.SecretsGetter
	projectAlertGroupLister v3.ProjectAlertGroupLister
	clusterAlertGroupLister v3.ClusterAlertGroupLister
	projectAlertRuleLister  v3.ProjectAlertRuleLister
	clusterAlertRuleLister  v3.ClusterAlertRuleLister
	notifierLister          v3.NotifierLister
	clusterName             string
	alertManager            *manager.AlertManager
	operatorCRDManager      *manager.PromOperatorCRDManager
}

func (d *ConfigSyncer) ProjectGroupSync(key string, alert *v3.ProjectAlertGroup) (runtime.Object, error) {
	return nil, d.sync()
}

func (d *ConfigSyncer) ClusterGroupSync(key string, alert *v3.ClusterAlertGroup) (runtime.Object, error) {
	return nil, d.sync()
}

func (d *ConfigSyncer) ProjectRuleSync(key string, alert *v3.ProjectAlertRule) (runtime.Object, error) {
	return nil, d.sync()
}

func (d *ConfigSyncer) ClusterRuleSync(key string, alert *v3.ClusterAlertRule) (runtime.Object, error) {
	return nil, d.sync()
}

func (d *ConfigSyncer) NotifierSync(key string, alert *v3.Notifier) (runtime.Object, error) {
	return nil, d.sync()
}

//sync: update the secret which store the configuration of alertmanager given the latest configured notifiers and alerts rules.
//For each alert, it will generate a route and a receiver in the alertmanager's configuration file, for metric rules it will update operator crd also.
func (d *ConfigSyncer) sync() error {
	if d.alertManager.IsDeploy == false {
		return nil
	}

	if _, err := d.alertManager.GetAlertManagerEndpoint(); err != nil {
		return err
	}
	notifiers, err := d.notifierLister.List("", labels.NewSelector())
	if err != nil {
		return errors.Wrapf(err, "List notifiers")
	}

	clusterAlertRules, err := d.clusterAlertRuleLister.List("", labels.NewSelector())
	if err != nil {
		return errors.Wrapf(err, "List cluster alert rules")
	}

	projectAlertRules, err := d.projectAlertRuleLister.List("", labels.NewSelector())
	if err != nil {
		return errors.Wrapf(err, "List project alert rules")
	}

	cAlertsMap := map[string][]*v3.ClusterAlertRule{}
	cAlertsKey := []string{}
	for _, alert := range clusterAlertRules {
		if alert.Status.AlertState != "inactive" {
			cAlertsMap[alert.Spec.GroupName] = append(cAlertsMap[alert.Spec.GroupName], alert)
		}
	}

	for k := range cAlertsMap {
		cAlertsKey = append(cAlertsKey, k)
	}
	sort.Strings(cAlertsKey)

	pAlertsMap := map[string]map[string][]*v3.ProjectAlertRule{}
	pAlertsKey := []string{}
	for _, alert := range projectAlertRules {
		if controller.ObjectInCluster(d.clusterName, alert) {
			if alert.Status.AlertState != "inactive" {
				_, projectName := ref.Parse(alert.Spec.ProjectName)
				if _, ok := pAlertsMap[projectName]; !ok {
					pAlertsMap[projectName] = make(map[string][]*v3.ProjectAlertRule)
				}
				pAlertsMap[projectName][alert.Spec.GroupName] = append(pAlertsMap[projectName][alert.Spec.GroupName], alert)
			}
		}
	}
	for k := range pAlertsMap {
		pAlertsKey = append(pAlertsKey, k)
	}
	sort.Strings(pAlertsKey)

	if err := d.addClusterAlert2Operator(cAlertsMap, cAlertsKey); err != nil {
		return err
	}

	if err := d.addProjectAlert2Operator(pAlertsMap, pAlertsKey); err != nil {
		return err
	}

	config := manager.GetAlertManagerDefaultConfig()
	config.Global.PagerdutyURL = "https://events.pagerduty.com/generic/2010-04-15/create_event.json"

	if err = d.addClusterAlert2Config(config, cAlertsMap, cAlertsKey, notifiers); err != nil {
		return err
	}

	if err = d.addProjectAlert2Config(config, pAlertsMap, pAlertsKey, notifiers); err != nil {
		return err
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return errors.Wrapf(err, "Marshal secrets")
	}

	altermanagerAppName, altermanagerAppNamespace := monitorutil.ClusterAlertManagerInfo()
	secretClient := d.secretsGetter.Secrets(altermanagerAppNamespace)
	secretName := common.GetAlertManagerSecretName(altermanagerAppName)
	configSecret, err := secretClient.Get(secretName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrapf(err, "Get secrets")
	}

	if string(configSecret.Data["alertmanager.yaml"]) != string(data) {
		newConfigSecret := configSecret.DeepCopy()
		newConfigSecret.Data["alertmanager.yaml"] = data
		newConfigSecret.Data["notification.tmpl"] = []byte(deployer.NotificationTmpl)

		_, err = secretClient.Update(newConfigSecret)
		if err != nil {
			return errors.Wrapf(err, "Update secrets")
		}

	} else {
		logrus.Debug("The config stay the same, will not update the secret")
	}

	return nil
}

func (d *ConfigSyncer) getNotifier(id string, notifiers []*v3.Notifier) *v3.Notifier {

	for _, n := range notifiers {
		if d.clusterName+":"+n.Name == id {
			return n
		}
	}

	return nil
}

func (d *ConfigSyncer) addProjectAlert2Operator(projectGroups map[string]map[string][]*v3.ProjectAlertRule, keys []string) error {
	for _, projectID := range keys {
		groupRules := projectGroups[projectID]
		_, projectName := ref.Parse(projectID)
		_, namespace := monitorutil.ProjectMonitoringInfo(projectName)
		promRule := d.operatorCRDManager.GetDefaultPrometheusRule(namespace, projectName)

		var groupIDs []string
		for k := range groupRules {
			groupIDs = append(groupIDs, k)
		}
		sort.Strings(groupIDs)

		var enabled bool
		for _, groupID := range groupIDs {
			enabled = true
			alertRules := groupRules[groupID]
			ruleGroup := d.operatorCRDManager.GetRuleGroup(groupID)
			for _, alertRule := range alertRules {
				if alertRule.Spec.MetricRule != nil {
					ruleID := common.GetRuleID(alertRule.Spec.GroupName, alertRule.Name)
					promRule := manager.Metric2Rule(groupID, ruleID, alertRule.Spec.Severity, alertRule.Spec.DisplayName, d.clusterName, alertRule.Spec.MetricRule)
					d.operatorCRDManager.AddRule(ruleGroup, promRule)
				}
			}
			d.operatorCRDManager.AddRuleGroup(promRule, *ruleGroup)
		}

		if !enabled {
			continue
		}

		if err := d.operatorCRDManager.SyncPrometheusRule(promRule); err != nil {
			return err
		}
	}

	return nil
}

func (d *ConfigSyncer) addClusterAlert2Operator(groupRules map[string][]*v3.ClusterAlertRule, keys []string) error {
	var enabled bool
	_, namespace := monitorutil.ClusterMonitoringInfo()
	promRule := d.operatorCRDManager.GetDefaultPrometheusRule(namespace, d.clusterName)

	for _, groupID := range keys {
		enabled = true
		ruleGroup := d.operatorCRDManager.GetRuleGroup(groupID)
		alertRules := groupRules[groupID]
		for _, alertRule := range alertRules {
			if alertRule.Spec.MetricRule != nil {
				ruleID := common.GetRuleID(alertRule.Spec.GroupName, alertRule.Name)
				promRule := manager.Metric2Rule(groupID, ruleID, alertRule.Spec.Severity, alertRule.Spec.DisplayName, d.clusterName, alertRule.Spec.MetricRule)
				d.operatorCRDManager.AddRule(ruleGroup, promRule)
			}
		}
		d.operatorCRDManager.AddRuleGroup(promRule, *ruleGroup)
	}

	if !enabled {
		return nil
	}

	return d.operatorCRDManager.SyncPrometheusRule(promRule)
}

func (d *ConfigSyncer) addProjectAlert2Config(config *alertconfig.Config, projectGroups map[string]map[string][]*v3.ProjectAlertRule, keys []string, notifiers []*v3.Notifier) error {
	for _, projectName := range keys {
		groups := projectGroups[projectName]
		var groupIDs []string
		for groupID := range groups {
			groupIDs = append(groupIDs, groupID)
		}
		sort.Strings(groupIDs)

		for _, groupID := range groupIDs {
			rules := groups[groupID]
			_, groupName := ref.Parse(groupID)
			group, err := d.projectAlertGroupLister.Get(projectName, groupName)
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("get project alert group %s:%s failed, %v", projectName, groupName, err)
			}

			if group == nil {
				continue
			}

			receiver := &alertconfig.Receiver{Name: groupID}

			exist := d.addRecipients(notifiers, receiver, group.Spec.Recipients)

			if exist {
				config.Receivers = append(config.Receivers, receiver)
				r1 := d.newRoute(map[string]string{"group_id": groupID}, defaultGroupWait, defaultRepeatInterval, defaultGroupInterval)

				for _, alert := range rules {
					if alert.Status.AlertState == "inactive" {
						continue
					}

					if alert.Spec.PodRule != nil || alert.Spec.WorkloadRule != nil || alert.Spec.MetricRule != nil {
						ruleID := common.GetRuleID(groupID, alert.Name)
						d.addRule(ruleID, r1, alert.Spec.CommonRuleField)
					}

				}
				d.appendRoute(config.Route, r1)
			}
		}
	}

	return nil
}

func (d *ConfigSyncer) addClusterAlert2Config(config *alertconfig.Config, alerts map[string][]*v3.ClusterAlertRule, keys []string, notifiers []*v3.Notifier) error {
	for _, groupID := range keys {
		groupRules := alerts[groupID]
		_, groupName := ref.Parse(groupID)

		receiver := &alertconfig.Receiver{Name: groupID}

		group, err := d.clusterAlertGroupLister.Get(d.clusterName, groupName)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get cluster alert group %s:%s failed, %v", d.clusterName, groupName, err)
		}

		if group == nil {
			continue
		}

		exist := d.addRecipients(notifiers, receiver, group.Spec.Recipients)

		if exist {
			config.Receivers = append(config.Receivers, receiver)
			r1 := d.newRoute(map[string]string{"group_id": groupID}, defaultGroupWait, defaultRepeatInterval, defaultGroupInterval)
			for _, alert := range groupRules {
				if alert.Status.AlertState == "inactive" {
					continue
				}
				ruleID := common.GetRuleID(groupID, alert.Name)

				if alert.Spec.EventRule != nil {
					r2 := d.newRoute(map[string]string{"alert_type": "event", "rule_id": ruleID}, defaultGroupWait, defaultRepeatInterval, eventGroupInterval)
					d.appendRoute(r1, r2) //todo: better not overwrite interval for each, if the interval is same as above, should not add interval field
				}

				if alert.Spec.MetricRule != nil || alert.Spec.SystemServiceRule != nil || alert.Spec.NodeRule != nil {
					d.addRule(ruleID, r1, alert.Spec.CommonRuleField)
				}

			}

			d.appendRoute(config.Route, r1)
		}
	}
	return nil
}

func (d *ConfigSyncer) addRule(ruleID string, route *alertconfig.Route, comm v3.CommonRuleField) {
	r2 := d.newRoute(map[string]string{"rule_id": ruleID}, comm.GroupWaitSeconds, comm.GroupIntervalSeconds, comm.RepeatIntervalSeconds)
	d.appendRoute(route, r2)
}

func (d *ConfigSyncer) newRoute(match map[string]string, groupWait, groupInterval, repeatInterval int) *alertconfig.Route {
	route := &alertconfig.Route{
		Receiver: match["group_id"],
		Match:    match,
	}

	gw := model.Duration(time.Duration(groupWait) * time.Second)
	route.GroupWait = &gw
	ri := model.Duration(time.Duration(repeatInterval) * time.Second)
	route.RepeatInterval = &ri

	if groupInterval != defaultGroupInterval {
		gi := model.Duration(time.Duration(groupInterval) * time.Second)
		route.GroupInterval = &gi
	}
	return route
}

func (d *ConfigSyncer) appendRoute(route *alertconfig.Route, subRoute *alertconfig.Route) {
	if route.Routes == nil {
		route.Routes = []*alertconfig.Route{}
	}
	route.Routes = append(route.Routes, subRoute)
}

func (d *ConfigSyncer) addRecipients(notifiers []*v3.Notifier, receiver *alertconfig.Receiver, recipients []v3.Recipient) bool {
	receiverExist := false
	for _, r := range recipients {
		if r.NotifierName != "" {
			notifier := d.getNotifier(r.NotifierName, notifiers)
			if notifier == nil {
				logrus.Debugf("Can not find the notifier %s", r.NotifierName)
				continue
			}

			if notifier.Spec.PagerdutyConfig != nil {
				pagerduty := &alertconfig.PagerdutyConfig{
					ServiceKey:  alertconfig.Secret(notifier.Spec.PagerdutyConfig.ServiceKey),
					Description: `{{ template "rancher.title" . }}`,
				}
				if r.Recipient != "" {
					pagerduty.ServiceKey = alertconfig.Secret(r.Recipient)
				}
				receiver.PagerdutyConfigs = append(receiver.PagerdutyConfigs, pagerduty)
				receiverExist = true

			} else if notifier.Spec.WebhookConfig != nil {
				webhook := &alertconfig.WebhookConfig{
					URL: notifier.Spec.WebhookConfig.URL,
				}
				if r.Recipient != "" {
					webhook.URL = r.Recipient
				}
				receiver.WebhookConfigs = append(receiver.WebhookConfigs, webhook)
				receiverExist = true
			} else if notifier.Spec.SlackConfig != nil {
				slack := &alertconfig.SlackConfig{
					APIURL:    alertconfig.Secret(notifier.Spec.SlackConfig.URL),
					Channel:   notifier.Spec.SlackConfig.DefaultRecipient,
					Text:      `{{ template "slack.text" . }}`,
					Title:     `{{ template "rancher.title" . }}`,
					TitleLink: "",
					Color:     `{{ if eq (index .Alerts 0).Labels.severity "critical" }}danger{{ else if eq (index .Alerts 0).Labels.severity "warning" }}warning{{ else }}good{{ end }}`,
				}
				if r.Recipient != "" {
					slack.Channel = r.Recipient
				}
				receiver.SlackConfigs = append(receiver.SlackConfigs, slack)
				receiverExist = true

			} else if notifier.Spec.SMTPConfig != nil {
				header := map[string]string{}
				header["Subject"] = `{{ template "rancher.title" . }}`
				email := &alertconfig.EmailConfig{
					Smarthost:    notifier.Spec.SMTPConfig.Host + ":" + strconv.Itoa(notifier.Spec.SMTPConfig.Port),
					AuthPassword: alertconfig.Secret(notifier.Spec.SMTPConfig.Password),
					AuthUsername: notifier.Spec.SMTPConfig.Username,
					RequireTLS:   &notifier.Spec.SMTPConfig.TLS,
					To:           notifier.Spec.SMTPConfig.DefaultRecipient,
					Headers:      header,
					From:         notifier.Spec.SMTPConfig.Sender,
					HTML:         `{{ template "email.text" . }}`,
				}
				if r.Recipient != "" {
					email.To = r.Recipient
				}
				receiver.EmailConfigs = append(receiver.EmailConfigs, email)
				receiverExist = true
			}

		}
	}

	return receiverExist

}

func includeProjectMetrics(projectAlerts []*v3.ProjectAlertRule) bool {
	for _, v := range projectAlerts {
		if v.Spec.MetricRule != nil {
			return true
		}
	}
	return false
}
