package utils

// Reconciling instructions.
const (
	ReconcileNow    = 0
	StopReconciling = 1
	DontReconcile   = 2
)

// Finalizers
const (
	CleanupFinalizer = "ran.openshift.io/ibu-cleanup-finalizer"
)
