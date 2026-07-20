import axios from 'api';
import { ErrorResponseHandlerV2 } from 'api/ErrorResponseHandlerV2';
import { AxiosError } from 'axios';
import { ErrorV2Resp, SuccessResponseV2 } from 'types/api';
import { SLOReport } from 'types/api/slo';

const getSLOs = async (): Promise<SuccessResponseV2<SLOReport[]>> => {
	try {
		const response = await axios.get<{ data: SLOReport[] }>('/slo');
		return {
			httpStatusCode: response.status,
			data: response.data.data,
		};
	} catch (error) {
		ErrorResponseHandlerV2(error as AxiosError<ErrorV2Resp>);
		throw error;
	}
};

export default getSLOs;
