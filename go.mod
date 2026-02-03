module github.com/openshift-kni/lifecycle-agent

go 1.25.3

require (
	github.com/containers/image/v5 v5.36.2
	github.com/coreos/go-semver v0.3.1
	github.com/coreos/ignition/v2 v2.25.1
	github.com/go-logr/logr v1.4.3
	github.com/google/go-cmp v0.7.0
	github.com/k8snetworkplumbingwg/sriov-network-operator v1.6.0
	github.com/openshift/api v0.0.0-20251125174858-5cf710f68a92
	github.com/openshift/assisted-image-service v0.0.0-20240528153439-08336d4322dc
	github.com/openshift/library-go v0.0.0-20251120164824-14a789e09884
	github.com/openshift/local-storage-operator v0.0.0-20240119085755-569393ff42e1
	github.com/openshift/lvm-operator v0.0.0-20240627192035-bf9fc3ac7ee8
	github.com/operator-framework/api v0.39.0
	github.com/otiai10/copy v1.14.0
	github.com/pelletier/go-toml v1.9.5
	github.com/samber/lo v1.52.0
	github.com/sirupsen/logrus v1.9.4
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.11.1
	github.com/vmware-tanzu/velero v1.17.2
	go.uber.org/mock v0.6.0
	golang.org/x/crypto v0.47.0
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/api v0.35.0
	k8s.io/apiextensions-apiserver v0.35.0
	k8s.io/apimachinery v0.35.0
	k8s.io/client-go v0.35.0
	k8s.io/code-generator v0.35.0
	open-cluster-management.io/api v1.1.0
	open-cluster-management.io/config-policy-controller v0.17.0
	open-cluster-management.io/governance-policy-propagator v0.17.0
	sigs.k8s.io/controller-runtime v0.23.1
	sigs.k8s.io/yaml v1.6.0
)

require (
	cel.dev/expr v0.25.1 // indirect
	cyphar.com/go-pathrs v0.2.1 // indirect
	dario.cat/mergo v1.0.2 // indirect
	github.com/BurntSushi/toml v1.5.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/aws/aws-sdk-go-v2 v1.41.0 // indirect
	github.com/cavaliercoder/go-cpio v0.0.0-20180626203310-925f9528c45e // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/containers/storage v1.59.1 // indirect
	github.com/cyphar/filepath-securejoin v0.6.0 // indirect
	github.com/diskfs/go-diskfs v1.4.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/elliotwutingfeng/asciiset v0.0.0-20230602022725-51bbb787efab // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/fxamacker/cbor/v2 v2.9.0 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/swag/cmdutils v0.25.4 // indirect
	github.com/go-openapi/swag/conv v0.25.4 // indirect
	github.com/go-openapi/swag/fileutils v0.25.4 // indirect
	github.com/go-openapi/swag/jsonname v0.25.4 // indirect
	github.com/go-openapi/swag/jsonutils v0.25.4 // indirect
	github.com/go-openapi/swag/loading v0.25.4 // indirect
	github.com/go-openapi/swag/mangling v0.25.4 // indirect
	github.com/go-openapi/swag/netutils v0.25.4 // indirect
	github.com/go-openapi/swag/stringutils v0.25.4 // indirect
	github.com/go-openapi/swag/typeutils v0.25.4 // indirect
	github.com/go-openapi/swag/yamlutils v0.25.4 // indirect
	github.com/golang/mock v1.6.0 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/cel-go v0.27.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.27.7 // indirect
	github.com/moby/sys/capability v0.4.0 // indirect
	github.com/moby/sys/mountinfo v0.7.2 // indirect
	github.com/moby/sys/user v0.4.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/opencontainers/runtime-spec v1.2.1 // indirect
	github.com/pierrec/lz4/v4 v4.1.17 // indirect
	github.com/pkg/xattr v0.4.9 // indirect
	github.com/robfig/cron v1.2.0 // indirect
	github.com/stolostron/kubernetes-dependency-watches v0.10.2 // indirect
	github.com/ulikunitz/xz v0.5.14 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.65.0 // indirect
	go.opentelemetry.io/otel v1.40.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.40.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.40.0 // indirect
	go.opentelemetry.io/otel/metric v1.40.0 // indirect
	go.opentelemetry.io/otel/sdk v1.40.0 // indirect
	go.opentelemetry.io/otel/trace v1.40.0 // indirect
	go.opentelemetry.io/proto/otlp v1.9.0 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260202165425-ce8ad4cf556b // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260202165425-ce8ad4cf556b // indirect
	google.golang.org/grpc v1.78.0 // indirect
	gopkg.in/djherbis/times.v1 v1.3.0 // indirect
	gopkg.in/evanphx/json-patch.v4 v4.13.0 // indirect
	k8s.io/apiserver v0.35.0 // indirect
	k8s.io/component-base v0.35.0 // indirect
	k8s.io/gengo/v2 v2.0.0-20250922181213-ec3ebc5fd46b // indirect
	k8s.io/kube-aggregator v0.34.1 // indirect
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.34.0 // indirect
	sigs.k8s.io/kube-storage-version-migrator v0.0.6-0.20230721195810-5c8923c5ff96 // indirect
	sigs.k8s.io/randfill v1.0.0 // indirect
	sigs.k8s.io/structured-merge-diff/v6 v6.3.2-0.20260122202528-d9cc6641c482 // indirect
)

require (
	github.com/Masterminds/goutils v1.1.1 // indirect
	github.com/Masterminds/semver/v3 v3.4.0 // indirect
	github.com/Masterminds/sprig/v3 v3.3.0 // indirect
	github.com/ajeddeloh/go-json v0.0.0-20200220154158-5ae607161559 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/clarketm/json v1.17.1 // indirect
	github.com/coreos/fcct v0.5.0 // indirect
	github.com/coreos/go-json v0.0.0-20230131223807-18775e0fb4fb // indirect
	github.com/coreos/go-systemd v0.0.0-20190719114852-fd7a80b32e1f // indirect
	github.com/coreos/go-systemd/v22 v22.6.0 // indirect
	github.com/coreos/ign-converter v0.0.0-20230417193809-cee89ea7d8ff // indirect
	github.com/coreos/ignition v0.35.0 // indirect
	github.com/coreos/vcontext v0.0.0-20230201181013-d72178a18687 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/emicklei/go-restful/v3 v3.13.0 // indirect
	github.com/evanphx/json-patch/v5 v5.9.11 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/ghodss/yaml v1.0.1-0.20190212211648-25d852aebe32 // indirect
	github.com/go-logr/zapr v1.3.0 // indirect
	github.com/go-openapi/jsonpointer v0.22.4 // indirect
	github.com/go-openapi/jsonreference v0.21.4 // indirect
	github.com/go-openapi/swag v0.25.4 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/google/gnostic-models v0.7.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/huandu/xstrings v1.5.0 // indirect
	github.com/imdario/mergo v1.0.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/selinux v1.13.1
	github.com/openshift/client-go v0.0.0-20251125141819-b6281947c285 // indirect
	github.com/openshift/machine-config-operator v0.0.1-0.20250320230514-53e78f3692ee // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.67.5 // indirect
	github.com/prometheus/procfs v0.19.2 // indirect
	github.com/rh-ecosystem-edge/preinstall-utils v0.0.0-20241120105227-a01c7fe6b461
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/vincent-petithory/dataurl v1.0.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	go4.org v0.0.0-20200104003542-c7e774b10ea0 // indirect
	golang.org/x/exp v0.0.0-20260112195511-716be5621a96 // indirect
	golang.org/x/mod v0.32.0 // indirect
	golang.org/x/net v0.49.0 // indirect
	golang.org/x/oauth2 v0.34.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/term v0.39.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	golang.org/x/tools v0.41.0 // indirect
	gomodules.xyz/jsonpatch/v2 v2.5.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	k8s.io/klog/v2 v2.130.1 // indirect
	k8s.io/kube-openapi v0.0.0-20260127142750-a19766b6e2d4 // indirect
	k8s.io/utils v0.0.0-20260108192941-914a6e750570 // indirect
	sigs.k8s.io/json v0.0.0-20250730193827-2d320260d730 // indirect
)

// From the mergo project's README: "If the vanity URL is causing issues in
// your project due to a dependency pulling Mergo - it isn't a direct
// dependency in your project - it is recommended to use replace to pin the
// version to the last one with the old import URL:"
replace github.com/imdario/mergo => github.com/imdario/mergo v0.3.16
