export type SLOState = 'healthy' | 'unhealthy' | 'indeterminate';

export type SLIType =
	| 'ratio'
	| 'latency_threshold'
	| 'completeness'
	| 'grounded_answers';

export interface SLOReport {
	name: string;
	service: string;
	type: SLIType;
	/** Target as a fraction in the range 0..1 (for example 0.99). */
	target: number;
	/** Evaluation window such as "30d". */
	window: string;
	/** Measured SLI as a fraction 0..1. Only meaningful when state is not indeterminate. */
	sli: number;
	state: SLOState;
	/** Telemetry completeness 0..1 from the auditor gate. */
	completeness: number;
	/** Remaining error budget as a fraction of the total budget. */
	errorBudgetRemaining: number;
	/** Error-budget burn rate; 1.0 exhausts the budget by the end of the window. */
	burnRate: number;
}
