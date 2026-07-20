import { Empty, Spin, Table, Typography } from 'antd';
import { SLOReport } from 'types/api/slo';

import styles from './SLO.module.scss';
import { getSLOColumns } from './utils';
import { useSLOs } from './useSLOs';

function SLOContainer(): JSX.Element {
	const { data, isLoading, isError } = useSLOs();
	const reports = data?.data ?? [];

	if (isLoading) {
		return (
			<div className={styles.state} data-testid="slo-loading">
				<Spin />
			</div>
		);
	}

	if (isError) {
		return (
			<div className={styles.state} data-testid="slo-error">
				<Empty description="Could not load SLOs" />
			</div>
		);
	}

	return (
		<div className={styles.container} data-testid="slo-container">
			<div className={styles.header}>
				<Typography.Text className={styles.title}>
					SLOs & Error Budgets
				</Typography.Text>
				<Typography.Text className={styles.subtitle}>
					Reliability objectives evaluated from telemetry. An SLO reads
					Indeterminate when the telemetry needed to compute it is incomplete.
				</Typography.Text>
			</div>

			{reports.length === 0 ? (
				<div data-testid="slo-empty">
					<Empty description="No SLOs configured. Set SIGNOZ_SLO_CONFIG_PATH to an SLO YAML file." />
				</div>
			) : (
				<div data-testid="slo-table">
					<Table<SLOReport>
						rowKey={(r): string => `${r.service}:${r.name}`}
						columns={getSLOColumns()}
						dataSource={reports}
						pagination={false}
					/>
				</div>
			)}
		</div>
	);
}

export default SLOContainer;
