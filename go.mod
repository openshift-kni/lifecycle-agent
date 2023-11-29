module github.com/openshift-kni/lifecycle-agent

go 1.20

require (
	github.com/RHsyseng/cluster-relocation-operator v0.9.9
	github.com/go-bindata/go-bindata v3.1.2+incompatible
	github.com/go-logr/logr v1.2.4
	github.com/openshift/api v0.0.0-20231123212421-7955d3da79e8
	github.com/openshift/build-machinery-go v0.0.0-20230824093055-6a18da01283c
	github.com/openshift/library-go v0.0.0-20231027143522-b8cd45d2d2c8
	github.com/operator-framework/api v0.17.6
	github.com/otiai10/copy v1.14.0
	github.com/sirupsen/logrus v1.9.3
	github.com/spf13/cobra v1.7.0
	github.com/stretchr/testify v1.8.4
	github.com/vmware-tanzu/velero v1.12.0
	go.etcd.io/etcd/client/v3 v3.5.10
	go.uber.org/mock v0.3.0
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/api v0.28.2
	k8s.io/apiextensions-apiserver v0.28.2
	k8s.io/apimachinery v0.28.2
	k8s.io/client-go v0.28.2
	k8s.io/code-generator v0.28.2
	open-cluster-management.io/api v0.11.0
	sigs.k8s.io/controller-runtime v0.16.1
	sigs.k8s.io/yaml v1.3.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/coreos/go-systemd/v22 v22.5.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/emicklei/go-restful/v3 v3.10.2 // indirect
	github.com/evanphx/json-patch v5.6.0+incompatible // indirect
	github.com/evanphx/json-patch/v5 v5.6.0 // indirect
	github.com/fsnotify/fsnotify v1.6.0 // indirect
	github.com/go-logr/zapr v1.2.4 // indirect
	github.com/go-openapi/jsonpointer v0.19.6 // indirect
	github.com/go-openapi/jsonreference v0.20.2 // indirect
	github.com/go-openapi/swag v0.22.4 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/google/gnostic-models v0.6.8 // indirect
	github.com/google/go-cmp v0.5.9 // indirect
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/google/pprof v0.0.0-20230323073829-e72429f035bd // indirect
	github.com/google/uuid v1.3.1 // indirect
	github.com/imdario/mergo v0.3.16 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.4 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/prometheus/client_golang v1.16.0 // indirect
	github.com/prometheus/client_model v0.4.0 // indirect
	github.com/prometheus/common v0.44.0 // indirect
	github.com/prometheus/procfs v0.10.1 // indirect
	github.com/spf13/pflag v1.0.6-0.20210604193023-d5e0c0615ace // indirect
	github.com/stolostron/cluster-lifecycle-api v0.0.0-20220621134646-8b67f2e6afed // indirect
	github.com/stolostron/klusterlet-addon-controller v0.0.0-20230528112800-a466a2368df4 // indirect
	go.etcd.io/etcd/api/v3 v3.5.10 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.10 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.26.0 // indirect
	golang.org/x/exp v0.0.0-20230817173708-d852ddb80c63 // indirect
	golang.org/x/mod v0.13.0 // indirect
	golang.org/x/net v0.17.0 // indirect
	golang.org/x/oauth2 v0.12.0 // indirect
	golang.org/x/sync v0.4.0 // indirect
	golang.org/x/sys v0.13.0 // indirect
	golang.org/x/term v0.13.0 // indirect
	golang.org/x/text v0.13.0 // indirect
	golang.org/x/time v0.3.0 // indirect
	golang.org/x/tools v0.14.0 // indirect
	gomodules.xyz/jsonpatch/v2 v2.4.0 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	google.golang.org/genproto v0.0.0-20230711160842-782d3b101e98 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20230711160842-782d3b101e98 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230711160842-782d3b101e98 // indirect
	google.golang.org/grpc v1.58.3 // indirect
	google.golang.org/protobuf v1.31.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	k8s.io/component-base v0.28.2 // indirect
	k8s.io/gengo v0.0.0-20220902162205-c0856e24416d // indirect
	k8s.io/klog/v2 v2.100.1 // indirect
	k8s.io/kube-openapi v0.0.0-20230717233707-2695361300d9 // indirect
	k8s.io/utils v0.0.0-20230726121419-3b25d923346b // indirect
	sigs.k8s.io/json v0.0.0-20221116044647-bc3834ca7abd // indirect
	sigs.k8s.io/structured-merge-diff/v4 v4.2.3 // indirect
)

//replace github.com/vmware-tanzu/velero => github.com/openshift/velero v0.10.2-0.20231002153713-b470b0211e1f
