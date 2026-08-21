package daemon

// LogFile is the daemon's own narration log: the file `tbx logs` reads, the one
// the CLI's spawn path redirects tbxd's stderr into, and the one every runbook
// points operators at. The CLI and the daemon must agree on the name, so it is
// declared once here rather than spelled out on both sides.
const LogFile = "tbxd.log"

// KubernetesLogFile holds everything the Kubernetes client libraries say. It is
// kept out of LogFile so klog's lines cannot crowd the daemon's own narration
// (#401).
const KubernetesLogFile = "tbxd.k8s.log"
