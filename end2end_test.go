package k8svpcnet

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/pkg/errors"

	"k8s.io/api/core/v1"
)

// This is the big 'ol end2end test. It tests networking functionality on a
// cluster. Assumes that it's running the latest release, and that loadbalancer
// services work. It's based on the assumption that if inbound pod connections
// works, and outbound ICMP works things are probably good. Behind the scenes
// this means NodePorts, pod2pod comms (i.e to kube-dns) and communications
// outside the cluster are OK. Nice to add later - comms to pods on same node
// vs. across nodes, for now can work on the assumption that test clusters are 2
// nodes so therefore dns lookups (i.e kube-dns) would trigger this.

var runEnd2End bool

func init() {
	flag.BoolVar(&runEnd2End, "run-e2e", false, "Run the end2end tests. These will target the cluster kubectl defaults too")
	flag.Parse()
}

func TestEnd2End(t *testing.T) {
	if !runEnd2End {
		t.Skip("-run-e2e not flagged in, skipping end2end test")
	}

	defer func() {
		kubectl("delete", "deployment", "e2e-nginx")
		kubectl("delete", "service", "e2e-nginx")
	}()

	t.Log("Deploying k8s-vpcnet to cluster")

	kubectlApply(t, nginxDeployment)
	kubectlApply(t, nginxService)

	// Wait for the ELB to be provisioned
	var elbHost string
	for i := 0; i < 120; i++ {
		svc := v1.Service{}
		kubectlGet(t, &svc, "service", "e2e-nginx")
		if len(svc.Status.LoadBalancer.Ingress) == 1 &&
			svc.Status.LoadBalancer.Ingress[0].Hostname != "" {
			elbHost = svc.Status.LoadBalancer.Ingress[0].Hostname
			break
		}
		time.Sleep(1 * time.Second)
	}

	// Wait for the ELB to be up and responding to requests
	var nginxReady bool
	for i := 0; i < 240; i++ {
		resp, err := http.Get("http://" + elbHost)
		if err != nil {
			t.Logf("Get ELB returned error... [%+v]", err)
			time.Sleep(2 * time.Second)
			continue
		}
		_ = resp.Body.Close()
		log.Print("ELB responded correctly")
		nginxReady = true
		break
	}
	if !nginxReady {
		t.Errorf("Nginx serviced failed to become ready in 4 minutes. Service/nodeport OK?")
	}

	pods := v1.PodList{}
	kubectlGet(t, &pods, "pod", "-l", "app=e2e-nginx")
	if len(pods.Items) != 2 {
		t.Fatalf("Consistency error - expected 2 nginx pods, got %d", len(pods.Items))
	}

	// Exec a ICMP ping to an external host in the container
	for _, p := range pods.Items {
		t.Logf("Pinging google.com from %s", p.Name)
		_, err := kubectl("exec", p.Name, "--", "ping", "-c", "2", "google.com")
		if err != nil {
			t.Errorf("Error pinging google.com from container [%+v]", err)
		}
	}

	for _, tc := range []struct {
		pod    string
		target string
	}{
		{
			pod:    pods.Items[0].Name,
			target: pods.Items[1].Status.PodIP,
		},
		{
			pod:    pods.Items[1].Name,
			target: pods.Items[0].Status.PodIP,
		},
	} {
		t.Logf("Pinging %s from %s", tc.target, tc.pod)
		_, err := kubectl("exec", tc.pod, "--", "ping", "-c", "2", tc.target)
		if err != nil {
			t.Errorf("Error pinging %s from pod %s [%+v]", tc.target, tc.pod, err)
		}
	}
}
func kubectlApply(t *testing.T, data string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("Error opening pipe: [%+v]", err)
	}
	_, err = stdin.Write([]byte(data))
	if err != nil {
		t.Fatalf("Error writing manifest to stdin pipe [%+v]", err)
	}
	err = stdin.Close()
	if err != nil {
		t.Fatalf("Error closing stdin pipe [%+v]", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Error running kubectl apply: [%+v]\n%s", err, string(out))
	}
	t.Logf("kubectl applied %#v", string(out))
}

func kubectlGet(t *testing.T, into interface{}, args ...string) {
	cmdArgs := []string{"get", "-o", "json"}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command("kubectl", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Error running kubectl [%+v]\n%s", err, string(out))
	}
	err = json.Unmarshal(out, into)
	if err != nil {
		t.Fatalf("Error parsing get JSON [%+v]", err)
	}
}

func kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.Wrapf(err, "Error running kubectl %v: \n%s", args, string(out))
	}
	return string(out), nil
}

const nginxDeployment = `
apiVersion: apps/v1beta1
kind: Deployment
metadata:
  name: e2e-nginx
spec:
  replicas: 2
  template:
    metadata:
      labels:
        app: e2e-nginx
    spec:
      containers:
      - name: nginx
        image: nginx:1.7.9
        ports:
        - containerPort: 80
`

const nginxService = `
apiVersion: v1
kind: Service
metadata:
  name: e2e-nginx
  annotations:
    service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled: "true"
spec:
  type: LoadBalancer
  ports:
  - port: 80
    targetPort: 80
  selector:
    app: e2e-nginx
`
