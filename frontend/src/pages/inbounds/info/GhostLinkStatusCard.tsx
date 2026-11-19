import { Alert, Descriptions, Skeleton, Tag } from 'antd';
import { useTranslation } from 'react-i18next';
import { useQuery } from '@tanstack/react-query';

import { HttpUtil } from '@/utils';
import {
  classifyGhostLinkService,
  type GhostLinkStatusData,
} from '@/lib/inbounds/ghostlink-status';

export default function GhostLinkStatusCard({ active }: { active: boolean }) {
  const { t } = useTranslation();
  const query = useQuery({
    queryKey: ['ghostlink', 'status'],
    enabled: active,
    retry: false,
    refetchInterval: active ? 30_000 : false,
    queryFn: async () => {
      const msg = await HttpUtil.get('/panel/api/inbounds/ghostlink/status', undefined, {
        silent: true,
      });
      if (!msg?.success || !msg.obj || typeof msg.obj !== 'object') {
        throw new Error(msg?.msg || t('pages.inbounds.ghostLinkStatus.unavailable'));
      }
      return msg.obj as GhostLinkStatusData;
    },
  });

  if (!active) return null;
  if (query.isPending) return <Skeleton active paragraph={{ rows: 3 }} />;
  if (query.isError) {
    return (
      <Alert
        type="error"
        showIcon
        title={t('pages.inbounds.ghostLinkStatus.unavailable')}
        description={
          query.error instanceof Error
            ? query.error.message
            : t('pages.inbounds.ghostLinkStatus.unavailable')
        }
      />
    );
  }
  const data = query.data;
  if (!data) return null;

  const revisionMatches =
    !!data.desiredRevision && !!data.activeRevision && data.desiredRevision === data.activeRevision;
  const userRevisionMatches =
    !!data.desiredUserRevision &&
    !!data.runtimeRevision &&
    data.desiredUserRevision === data.runtimeRevision;
  const serviceState = classifyGhostLinkService(data);

  return (
    <div className="ghostlink-status-card">
      <Descriptions
        size="small"
        bordered
        column={1}
        title={t('pages.inbounds.ghostLinkStatus.title')}
        items={[
          {
            key: 'service',
            label: t('pages.inbounds.ghostLinkStatus.service'),
            children: (
              <Tag
                title={
                  serviceState === 'healthy'
                    ? undefined
                    : t('pages.inbounds.ghostLinkStatus.notReady')
                }
                color={
                  serviceState === 'healthy'
                    ? 'green'
                    : serviceState === 'degraded'
                      ? 'orange'
                      : 'red'
                }
              >
                {serviceState === 'healthy'
                  ? t('pages.inbounds.ghostLinkStatus.ready')
                  : serviceState === 'degraded'
                    ? t('pages.inbounds.ghostLinkStatus.degraded')
                    : serviceState === 'stopped'
                      ? t('pages.inbounds.ghostLinkStatus.stopped')
                      : t('pages.inbounds.ghostLinkStatus.error')}
              </Tag>
            ),
          },
          {
            key: 'pid',
            label: 'MainPID',
            children: data.mainPid ? `${data.mainPid} / runtime ${data.runtimePid || '-'}` : '-',
          },
          {
            key: 'listen',
            label: t('pages.inbounds.ghostLinkStatus.listeners'),
            children: data.listen ? `TCP/H2 + UDP/H3 ${data.listen}` : '-',
          },
          {
            key: 'revision',
            label: t('pages.inbounds.ghostLinkStatus.revision'),
            children: (
              <Tag color={revisionMatches ? 'green' : 'orange'}>
                {data.activeRevision || '-'} / {data.desiredRevision || '-'}
              </Tag>
            ),
          },
          {
            key: 'userRevision',
            label: t('pages.inbounds.ghostLinkStatus.userRevision'),
            children: (
              <Tag color={userRevisionMatches ? 'green' : 'orange'}>
                {data.runtimeRevision || '-'} / {data.desiredUserRevision || '-'}
              </Tag>
            ),
          },
          {
            key: 'usage',
            label: t('pages.inbounds.ghostLinkStatus.usagePolicy'),
            children: `${data.usageState || '-'} / ${data.policyState || '-'}`,
          },
          ...(data.pendingOperationId
            ? [
                {
                  key: 'pending',
                  label: t('pages.inbounds.ghostLinkStatus.pending'),
                  children: `${data.pendingOperationId} (${data.pendingPhase || '-'})`,
                },
              ]
            : []),
          ...(data.reason
            ? [
                {
                  key: 'reason',
                  label: t('pages.inbounds.ghostLinkStatus.reason'),
                  children: data.reason,
                },
              ]
            : []),
          ...(data.usageErrorCode || data.policyErrorCode
            ? [
                {
                  key: 'errorCode',
                  label: t('pages.inbounds.ghostLinkStatus.error'),
                  children: data.policyErrorCode || data.usageErrorCode,
                },
              ]
            : []),
        ]}
      />
    </div>
  );
}
