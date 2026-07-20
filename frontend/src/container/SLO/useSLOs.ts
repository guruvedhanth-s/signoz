import getSLOs from 'api/slo/getSLOs';
import { AxiosError } from 'axios';
import { useQuery, UseQueryResult } from 'react-query';
import { ErrorV2Resp, SuccessResponseV2 } from 'types/api';
import { SLOReport } from 'types/api/slo';

export const SLO_LIST_QUERY_KEY = 'slo-list';

/**
 * useSLOs fetches the evaluated SLOs for the current organization. It refetches
 * every 30s so the trust state, error budget, and burn rate stay live during a
 * demo without a manual refresh.
 */
export function useSLOs(): UseQueryResult<
	SuccessResponseV2<SLOReport[]>,
	AxiosError<ErrorV2Resp>
> {
	return useQuery<SuccessResponseV2<SLOReport[]>, AxiosError<ErrorV2Resp>>(
		[SLO_LIST_QUERY_KEY],
		getSLOs,
		{ refetchInterval: 30_000 },
	);
}
