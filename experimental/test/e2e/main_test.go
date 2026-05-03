package graphcontroller_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ellistarn/kro/experimental/stdlib"
	"github.com/ellistarn/kro/experimental/test/testenv"
)

var (
	env         *testenv.Environment
	k8sClient   client.Client
	ctx         context.Context
	cancel      context.CancelFunc
	metricsAddr string // address of the controller's /metrics endpoint
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))
	flag.Parse()
	ctx, cancel = context.WithCancel(context.Background())

	// -----------------------------------------------------------------------
	// 1. Start envtest + controller binary via testenv.
	// -----------------------------------------------------------------------
	env = &testenv.Environment{}
	cfg, err := env.Start()
	if err != nil {
		panic("starting test environment: " + err.Error())
	}

	metricsAddr = env.MetricsAddr
	os.Setenv("PPROF_ADDR", env.PProfAddr)
	os.Setenv("CONTROLLER_PID", strconv.Itoa(env.ControllerPID))

	// Ensure cleanup on panic, signal, or test timeout.
	defer func() {
		cancel()
		env.Stop()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nreceived %s, stopping environment\n", sig)
		cancel()
		env.Stop()
		os.Exit(1)
	}()

	if f := flag.Lookup("test.timeout"); f != nil {
		if d, err := time.ParseDuration(f.Value.String()); err == nil && d > 5*time.Second {
			time.AfterFunc(d-5*time.Second, func() {
				fmt.Fprintln(os.Stderr, "test timeout approaching, stopping environment")
				cancel()
				env.Stop()
				os.Exit(1)
			})
		}
	}

	// -----------------------------------------------------------------------
	// 2. Install test-specific CRDs (RGD, SimpleApp). Chart CRDs (Graph,
	//    GraphRevision, Kind) are already loaded by envtest from crds/.
	// -----------------------------------------------------------------------
	k8sClient, err = env.Client()
	if err != nil {
		panic("creating client: " + err.Error())
	}

	testCRDs := []*apiextensionsv1.CustomResourceDefinition{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "resourcegraphdefinitions.test.kro.run"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "test.kro.run",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural:   "resourcegraphdefinitions",
					Singular: "resourcegraphdefinition",
					Kind:     "ResourceGraphDefinition",
					ListKind: "ResourceGraphDefinitionList",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type:                   "object",
							XPreserveUnknownFields: ptr.To(true),
						},
					},
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "simpleapps.test.kro.run"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "test.kro.run",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural:   "simpleapps",
					Singular: "simpleapp",
					Kind:     "SimpleApp",
					ListKind: "SimpleAppList",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type:                   "object",
							XPreserveUnknownFields: ptr.To(true),
						},
					},
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
				}},
			},
		},
		// Strict status validation CRD — used by T1.4 to produce a
		// status-only validation failure: spec accepts anything, but
		// status.phase is constrained to an enum.
		{
			ObjectMeta: metav1.ObjectMeta{Name: "strictstatuses.test.kro.run"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "test.kro.run",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural:   "strictstatuses",
					Singular: "strictstatus",
					Kind:     "StrictStatus",
					ListKind: "StrictStatusList",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type:                   "object",
									XPreserveUnknownFields: ptr.To(true),
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"phase": {
											Type: "string",
											Enum: []apiextensionsv1.JSON{
												{Raw: []byte(`"Running"`)},
												{Raw: []byte(`"Stopped"`)},
												{Raw: []byte(`"Failed"`)},
											},
										},
										"message": {Type: "string"},
									},
								},
							},
						},
					},
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
				}},
			},
		},
	}
	for _, crd := range testCRDs {
		if err := k8sClient.Create(ctx, crd); err != nil {
			panic("creating CRD " + crd.Name + ": " + err.Error())
		}
	}
	for _, crd := range testCRDs {
		if err := waitForCRD(ctx, k8sClient, crd.Name); err != nil {
			panic("waiting for CRD " + crd.Name + ": " + err.Error())
		}
	}

	// -----------------------------------------------------------------------
	// 3. Apply stdlib — the controller is pure substrate, tests install
	//    what they need.
	// -----------------------------------------------------------------------
	if err := stdlib.Apply(ctx, logf.Log.WithName("stdlib"), cfg); err != nil {
		panic("applying stdlib: " + err.Error())
	}

	// -----------------------------------------------------------------------
	// 4. Run tests.
	// -----------------------------------------------------------------------
	code := m.Run()

	// -----------------------------------------------------------------------
	// 5. Teardown.
	// -----------------------------------------------------------------------
	cancel()
	env.Stop()
	os.Exit(code)
}
