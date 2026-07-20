import { Tag, TableColumnsType as ColumnsType, Tooltip } from 'antd';
import { SLOReport, SLOState } from 'types/api/slo';

const STATE_META: Record<SLOState, { color: string; label: string }> = {
	healthy: { color: 'green', label: 'Healthy' },
	unhealthy: { color: 'red', label: 'Unhealthy' },
	indeterminate: { color: 'gold', label: 'Indeterminate' },
};

export function StateBadge({ state }: { state: SLOState }): JSX.Element {
	const meta = STATE_META[state];
	const tag = (
		<Tag color={meta.color} data-testid={`slo-state-${state}`}>
			{meta.label}
		</Tag>
	);
	if (state === 'indeterminate') {
		return (
			<Tooltip title="Telemetry is incomplete, so this SLO cannot be trusted. Fix instrumentation to make it measurable.">
				{tag}
			</Tooltip>
		);
	}
	return tag;
}

function formatPct(value: number): string {
	return `${(value * 100).toFixed(2)}%`;
}

const INDETERMINATE = '—';

export function getSLOColumns(): ColumnsType<SLOReport> {
	return [
		{
			title: 'SLO',
			dataIndex: 'name',
			key: 'name',
			render: (name: string): JSX.Element => (
				<span data-testid="slo-name">{name}</span>
			),
		},
		{ title: 'Service', dataIndex: 'service', key: 'service' },
		{
			title: 'State',
			dataIndex: 'state',
			key: 'state',
			render: (state: SLOState): JSX.Element => <StateBadge state={state} />,
		},
		{
			title: 'SLI',
			key: 'sli',
			render: (_, r): string =>
				r.state === 'indeterminate' ? INDETERMINATE : formatPct(r.sli),
		},
		{
			title: 'Target',
			dataIndex: 'target',
			key: 'target',
			render: (target: number): string => formatPct(target),
		},
		{
			title: 'Error budget left',
			key: 'errorBudgetRemaining',
			render: (_, r): string =>
				r.state === 'indeterminate'
					? INDETERMINATE
					: formatPct(r.errorBudgetRemaining),
		},
		{
			title: 'Burn rate',
			key: 'burnRate',
			render: (_, r): string =>
				r.state === 'indeterminate' ? INDETERMINATE : `${r.burnRate.toFixed(2)}x`,
		},
		{
			title: 'Window',
			dataIndex: 'window',
			key: 'window',
		},
	];
}
