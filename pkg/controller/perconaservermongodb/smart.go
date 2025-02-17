package perconaservermongodb

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/percona/percona-server-mongodb-operator/pkg/apis/psmdb/v1"
	"github.com/percona/percona-server-mongodb-operator/pkg/psmdb"
	"github.com/percona/percona-server-mongodb-operator/pkg/psmdb/backup"
)

func (r *ReconcilePerconaServerMongoDB) smartUpdate(ctx context.Context, cr *api.PerconaServerMongoDB, sfs *appsv1.StatefulSet,
	replset *api.ReplsetSpec) error {

	log := logf.FromContext(ctx)
	if replset.Size == 0 {
		return nil
	}

	if cr.Spec.UpdateStrategy != api.SmartUpdateStatefulSetStrategyType {
		return nil
	}

	matchLabels := map[string]string{
		"app.kubernetes.io/name":       "percona-server-mongodb",
		"app.kubernetes.io/instance":   cr.Name,
		"app.kubernetes.io/replset":    replset.Name,
		"app.kubernetes.io/managed-by": "percona-server-mongodb-operator",
		"app.kubernetes.io/part-of":    "percona-server-mongodb",
	}

	label, ok := sfs.Labels["app.kubernetes.io/component"]
	if ok {
		matchLabels["app.kubernetes.io/component"] = label
	}

	list := corev1.PodList{}
	if err := r.client.List(ctx,
		&list,
		&k8sclient.ListOptions{
			Namespace:     cr.Namespace,
			LabelSelector: labels.SelectorFromSet(matchLabels),
		},
	); err != nil {
		return fmt.Errorf("get pod list: %v", err)
	}

	if !isSfsChanged(sfs, &list) {
		return nil
	}

	if cr.CompareVersion("1.4.0") < 0 {
		return nil
	}

	if cr.Spec.Sharding.Enabled && sfs.Name != cr.Name+"-"+api.ConfigReplSetName {
		cfgSfs := appsv1.StatefulSet{}
		err := r.client.Get(ctx, types.NamespacedName{Name: cr.Name + "-" + api.ConfigReplSetName, Namespace: cr.Namespace}, &cfgSfs)
		if err != nil {
			return errors.Wrapf(err, "get config statefulset %s/%s", cr.Namespace, cr.Name+"-"+api.ConfigReplSetName)
		}
		cfgList, err := psmdb.GetRSPods(ctx, r.client, cr, api.ConfigReplSetName)
		if err != nil {
			return errors.Wrap(err, "get cfg pod list")
		}
		if isSfsChanged(&cfgSfs, &cfgList) {
			log.Info("waiting for config RS update")
			return nil
		}
	}

	log.Info("StatefulSet is changed, starting smart update", "name", sfs.Name)

	if sfs.Status.ReadyReplicas < sfs.Status.Replicas {
		log.Info("can't start/continue 'SmartUpdate': waiting for all replicas are ready")
		return nil
	}

	isBackupRunning, err := r.isBackupRunning(ctx, cr)
	if err != nil {
		return errors.Wrap(err, "failed to check active backups")
	}
	if isBackupRunning {
		log.Info("can't start 'SmartUpdate': waiting for running backups to be finished")
		return nil
	}

	hasActiveJobs, err := backup.HasActiveJobs(ctx, r.newPBM, r.client, cr, backup.Job{}, backup.NotPITRLock)
	if err != nil {
		return errors.Wrap(err, "failed to check active jobs")
	}

	_, ok = sfs.Annotations[api.AnnotationRestoreInProgress]
	if !ok && hasActiveJobs {
		log.Info("can't start 'SmartUpdate': waiting for active jobs to be finished")
		return nil
	}

	if sfs.Name == cr.Name+"-"+api.ConfigReplSetName {
		err = r.disableBalancer(ctx, cr)
		if err != nil {
			return errors.Wrap(err, "failed to stop balancer")
		}
	}

	waitLimit := int(replset.LivenessProbe.InitialDelaySeconds)

	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Name > list.Items[j].Name
	})

	var primaryPod corev1.Pod
	for _, pod := range list.Items {
		isPrimary, err := r.isPodPrimary(ctx, cr, pod, replset)
		if err != nil {
			return errors.Wrap(err, "is pod primary")
		}
		if isPrimary {
			primaryPod = pod
			continue
		}

		log.Info("apply changes to secondary pod", "pod", pod.Name)

		updateRevision := sfs.Status.UpdateRevision
		if pod.Labels["app.kubernetes.io/component"] == "arbiter" {
			arbiterSfs, err := r.getArbiterStatefulset(ctx, cr, pod.Labels["app.kubernetes.io/replset"])
			if err != nil {
				return errors.Wrap(err, "failed to get arbiter statefulset")
			}

			updateRevision = arbiterSfs.Status.UpdateRevision
		}

		if err := r.applyNWait(ctx, cr, updateRevision, &pod, waitLimit); err != nil {
			return errors.Wrap(err, "failed to apply changes")
		}
	}

	// If the primary is external, we can't match it with a running pod and
	// it'll have an empty name
	if sfs.Labels["app.kubernetes.io/component"] != "nonVoting" && len(primaryPod.Name) > 0 {
		forceStepDown := replset.Size == 1
		log.Info("doing step down...", "force", forceStepDown)
		client, err := r.mongoClientWithRole(ctx, cr, *replset, api.RoleClusterAdmin)
		if err != nil {
			return fmt.Errorf("failed to get mongo client: %v", err)
		}

		defer func() {
			err := client.Disconnect(ctx)
			if err != nil {
				log.Error(err, "failed to close connection")
			}
		}()

		err = client.StepDown(ctx, forceStepDown)
		if err != nil {
			return errors.Wrap(err, "failed to do step down")
		}

		log.Info("apply changes to primary pod", "pod", primaryPod.Name)
		if err := r.applyNWait(ctx, cr, sfs.Status.UpdateRevision, &primaryPod, waitLimit); err != nil {
			return fmt.Errorf("failed to apply changes: %v", err)
		}
	}

	log.Info("smart update finished for statefulset", "statefulset", sfs.Name)

	return nil
}

func (r *ReconcilePerconaServerMongoDB) isPodPrimary(ctx context.Context, cr *api.PerconaServerMongoDB, pod corev1.Pod, rs *api.ReplsetSpec) (bool, error) {
	log := logf.FromContext(ctx)

	host, err := psmdb.MongoHost(ctx, r.client, cr, cr.Spec.ClusterServiceDNSMode, rs.Name, rs.Expose.Enabled, pod)
	if err != nil {
		return false, errors.Wrap(err, "failed to get mongo host")
	}
	mgoClient, err := r.standaloneClientWithRole(ctx, cr, api.RoleClusterAdmin, host)
	if err != nil {
		return false, errors.Wrap(err, "failed to create standalone client")
	}
	defer func() {
		err := mgoClient.Disconnect(ctx)
		if err != nil {
			log.Error(err, "failed to close connection")
		}
	}()

	isMaster, err := mgoClient.IsMaster(ctx)
	if err != nil {
		return false, errors.Wrap(err, "is master")
	}

	return isMaster.IsMaster, nil
}

func (r *ReconcilePerconaServerMongoDB) smartMongosUpdate(ctx context.Context, cr *api.PerconaServerMongoDB, sts *appsv1.StatefulSet) error {
	log := logf.FromContext(ctx)

	if cr.Spec.Sharding.Mongos.Size == 0 || cr.Spec.UpdateStrategy != api.SmartUpdateStatefulSetStrategyType {
		return nil
	}

	list, err := r.getMongosPods(ctx, cr)
	if err != nil {
		return errors.Wrap(err, "get mongos pods")
	}

	if !isSfsChanged(sts, &list) {
		return nil
	}

	log.Info("StatefulSet is changed, starting smart update", "name", sts.Name)

	if sts.Status.ReadyReplicas < sts.Status.Replicas {
		log.Info("can't start/continue 'SmartUpdate': waiting for all replicas are ready")
		return nil
	}

	isBackupRunning, err := r.isBackupRunning(ctx, cr)
	if err != nil {
		return errors.Wrap(err, "failed to check active backups")
	}
	if isBackupRunning {
		log.Info("can't start 'SmartUpdate': waiting for running backups to be finished")
		return nil
	}

	hasActiveJobs, err := backup.HasActiveJobs(ctx, r.newPBM, r.client, cr, backup.Job{}, backup.NotPITRLock)
	if err != nil {
		return errors.Wrap(err, "failed to check active jobs")
	}

	if hasActiveJobs {
		log.Info("can't start 'SmartUpdate': waiting for active jobs to be finished")
		return nil
	}

	waitLimit := int(cr.Spec.Sharding.Mongos.LivenessProbe.InitialDelaySeconds)

	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Name > list.Items[j].Name
	})

	for _, pod := range list.Items {
		if err := r.applyNWait(ctx, cr, sts.Status.UpdateRevision, &pod, waitLimit); err != nil {
			return errors.Wrap(err, "failed to apply changes")
		}
	}
	log.Info("smart update finished for mongos statefulset")

	return nil
}

func (r *ReconcilePerconaServerMongoDB) isStsListUpToDate(ctx context.Context, cr *api.PerconaServerMongoDB, stsList *appsv1.StatefulSetList) (bool, error) {
	for _, s := range stsList.Items {
		podList := new(corev1.PodList)
		if err := r.client.List(ctx, podList,
			&k8sclient.ListOptions{
				Namespace:     cr.Namespace,
				LabelSelector: labels.SelectorFromSet(s.Labels),
			}); err != nil {
			return false, errors.Errorf("failed to get statefulset %s pods: %v", s.Name, err)
		}
		if s.Status.UpdatedReplicas < s.Status.Replicas || isSfsChanged(&s, podList) {
			logf.FromContext(ctx).Info("StatefulSet is not up to date", "sts", s.Name)
			return false, nil
		}
	}
	return true, nil
}

func (r *ReconcilePerconaServerMongoDB) isAllSfsUpToDate(ctx context.Context, cr *api.PerconaServerMongoDB) (bool, error) {
	sfsList := appsv1.StatefulSetList{}
	if err := r.client.List(ctx, &sfsList,
		&k8sclient.ListOptions{
			Namespace: cr.Namespace,
			LabelSelector: labels.SelectorFromSet(map[string]string{
				"app.kubernetes.io/instance": cr.Name,
			}),
		},
	); err != nil {
		return false, errors.Wrap(err, "failed to get statefulset list")
	}

	return r.isStsListUpToDate(ctx, cr, &sfsList)
}

func (r *ReconcilePerconaServerMongoDB) applyNWait(ctx context.Context, cr *api.PerconaServerMongoDB, updateRevision string, pod *corev1.Pod, waitLimit int) error {
	if pod.ObjectMeta.Labels["controller-revision-hash"] == updateRevision {
		logf.FromContext(ctx).Info("Pod already updated", "pod", pod.Name)
	} else {
		if err := r.client.Delete(ctx, pod); err != nil {
			return errors.Wrap(err, "delete pod")
		}
	}

	if err := r.waitPodRestart(ctx, cr, updateRevision, pod, waitLimit); err != nil {
		return errors.Wrap(err, "wait pod restart")
	}

	return nil
}

func (r *ReconcilePerconaServerMongoDB) waitPodRestart(ctx context.Context, cr *api.PerconaServerMongoDB, updateRevision string, pod *corev1.Pod, waitLimit int) error {
	for i := 0; i < waitLimit; i++ {
		time.Sleep(time.Second * 1)

		err := r.client.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, pod)
		if err != nil && !k8sErrors.IsNotFound(err) {
			return errors.Wrap(err, "get pod")
		}

		// We update status in every loop to not wait until the end of smart update
		if err := r.updateStatus(ctx, cr, nil, api.AppStateInit); err != nil {
			return errors.Wrap(err, "update status")
		}

		ready := false
		for _, container := range pod.Status.ContainerStatuses {
			switch container.Name {
			case "mongod", "mongod-arbiter", "mongod-nv", "mongos":
				ready = container.Ready
			}
		}

		if pod.Status.Phase == corev1.PodRunning && pod.ObjectMeta.Labels["controller-revision-hash"] == updateRevision && ready {
			logf.FromContext(ctx).Info("Pod started", "pod", pod.Name)
			return nil
		}
	}

	return errors.New("reach pod wait limit")
}

func isSfsChanged(sfs *appsv1.StatefulSet, podList *corev1.PodList) bool {
	if sfs.Status.UpdateRevision == "" {
		return false
	}

	for _, pod := range podList.Items {
		if pod.Labels["app.kubernetes.io/component"] != sfs.Labels["app.kubernetes.io/component"] {
			continue
		}
		if pod.ObjectMeta.Labels["controller-revision-hash"] != sfs.Status.UpdateRevision {
			return true
		}
	}
	return false
}
