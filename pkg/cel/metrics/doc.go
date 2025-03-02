/*
Package metrics provides Prometheus metrics for monitoring CEL expression compilation and evaluation.

Common Usage Patterns:

1. Simple recording of compilation:

    timer := utils.NewCELCompilationTimer("ResourceGroup", "validation")
    prog, err := cel.Compile(expr)
    if err != nil {
        timer.ObserveFailure()
        return err
    }
    timer.ObserveSuccess()

2. Evaluation with the WithCELEvaluation helper:

    err := metrics.WithCELEvaluation("ResourceGroup", "validation", "condition", func() error {
        result, err := prog.Eval(vars)
        if err != nil {
            return err
        }
        // Process result
        return nil
    })

3. Manual tracking for more complex scenarios:

    metrics.IncrementActiveCELEvaluations("ResourceInstance", "transform")
    defer metrics.DecrementActiveCELEvaluations("ResourceInstance", "transform")
    
    start := time.Now()
    // ... perform evaluation ...
    elapsed := time.Since(start).Seconds()
    
    if err != nil {
        metrics.RecordCELEvaluationFailure("ResourceInstance", "transform", "patch")
        if isTimeoutError(err) {
            metrics.RecordCELEvaluationError("timeout", "ResourceInstance", "transform")
        } else {
            metrics.RecordCELEvaluationError("execution", "ResourceInstance", "transform")
        }
    } else {
        metrics.RecordCELEvaluationSuccess("ResourceInstance", "transform", "patch")
    }
    metrics.RecordCELEvaluationLatency("ResourceInstance", "transform", "patch", elapsed)

Available Labels:

- resource_type: Type of resource being processed (ResourceGroup, ResourceInstance)
- operation_type: Type of operation (validation, transformation, condition)
- expression_type: Purpose or type of the expression (condition, patch, defaulting)
- result: Outcome of the operation (success, failure)
- error_type: Type of error encountered (compilation, runtime, timeout)

*/
package metrics 