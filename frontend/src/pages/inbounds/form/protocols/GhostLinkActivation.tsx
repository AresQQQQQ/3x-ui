import { Alert, Button, Col, Descriptions, Input, Skeleton, Space } from 'antd';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { HttpUtil } from '@/utils';

type Activation = {
  ready: boolean;
  configured: boolean;
  status: string;
  request_id?: string;
  fingerprint?: { machine_id_hash: string; dmi_uuid_hash: string; mac_hashes: string[] };
};

const endpoint = '/panel/api/inbounds/ghostlink/activation';
const queryKey = ['ghostlink', 'activation'];

function useGhostLinkActivation() {
  return useQuery({
    queryKey,
    retry: false,
    gcTime: 0,
    refetchInterval: (query) => (query.state.data?.ready ? false : 3_000),
    queryFn: async () => {
      const response = await HttpUtil.get<Activation>(endpoint, undefined, { silent: true });
      if (!response.success || !response.obj) throw new Error(response.msg);
      return response.obj;
    },
  });
}

export function GhostLinkActivationBanner({
  onOpenActivation,
}: {
  onOpenActivation: () => void;
}) {
  const status = useGhostLinkActivation();
  const { t } = useTranslation();

  if (status.isError || !status.data || status.data.ready) return null;

  const data = status.data;
  const waiting = ['pending', 'downloading', 'verified'].includes(data.status);
  const statusDetail = data.configured
    ? data.status === 'inactive'
      ? t('pages.inbounds.ghostLinkActivation.bannerDescription')
      : t('pages.inbounds.ghostLinkActivation.' + data.status)
    : t('pages.inbounds.ghostLinkActivation.unconfigured');
  const description = data.request_id
    ? `${statusDetail} (${t('pages.inbounds.ghostLinkActivation.request')}: ${data.request_id})`
    : statusDetail;

  return (
    <Col span={24}>
      <Alert
        type={waiting ? 'info' : 'warning'}
        showIcon
        title={t('pages.inbounds.ghostLinkActivation.bannerTitle')}
        description={description}
        action={
          <Button size="small" type={waiting ? 'default' : 'primary'} onClick={onOpenActivation}>
            {t(
              waiting
                ? 'pages.inbounds.ghostLinkActivation.bannerViewStatus'
                : 'pages.inbounds.ghostLinkActivation.bannerAction',
            )}
          </Button>
        }
      />
    </Col>
  );
}

export default function GhostLinkActivation({
  onReadyChange,
}: {
  onReadyChange: (ready: boolean) => void;
}) {
  const status = useGhostLinkActivation();
  const ready = !!status.data?.ready && !status.isError;
  useEffect(() => {
    onReadyChange(ready);
    return () => onReadyChange(false);
  }, [onReadyChange, ready]);
  const { t } = useTranslation();
  const client = useQueryClient();
  const [note, setNote] = useState('');
  const activate = useMutation({
    mutationFn: async () => {
      const response = await HttpUtil.post<Activation>(endpoint, { note }, { silent: true });
      if (!response.success || !response.obj) throw new Error(response.msg);
      return response.obj;
    },
    onSuccess: (next) => {
      client.setQueryData(queryKey, next);
    },
  });

  if (ready) return null;
  if (status.isError) {
    return (
      <Alert
        type="error"
        showIcon
        style={{ marginBottom: 16 }}
        title={t('pages.inbounds.ghostLinkActivation.unavailable')}
      />
    );
  }
  if (!status.data) return <Skeleton active paragraph={{ rows: 3 }} style={{ marginBottom: 16 }} />;

  const data = status.data;
  const waiting = ['pending', 'downloading', 'verified'].includes(data.status);
  return (
    <Space orientation="vertical" size="middle" style={{ width: '100%', marginBottom: 16 }}>
      <Alert
        type={data.configured ? (waiting ? 'info' : 'warning') : 'warning'}
        showIcon
        title={t('pages.inbounds.ghostLinkActivation.title')}
        description={
          data.configured
            ? data.status === 'inactive'
              ? t('pages.inbounds.ghostLinkActivation.inactiveHint')
              : t('pages.inbounds.ghostLinkActivation.' + data.status)
            : t('pages.inbounds.ghostLinkActivation.unconfigured')
        }
      />
      {data.request_id && (
        <span>
          {t('pages.inbounds.ghostLinkActivation.request')} {data.request_id}
        </span>
      )}
      {data.fingerprint && (
        <Descriptions
          column={1}
          size="small"
          items={[
            {
              key: 'machine',
              label: t('pages.inbounds.ghostLinkActivation.machine'),
              children: data.fingerprint.machine_id_hash || '—',
            },
            {
              key: 'dmi',
              label: t('pages.inbounds.ghostLinkActivation.dmi'),
              children: data.fingerprint.dmi_uuid_hash || '—',
            },
            {
              key: 'mac',
              label: t('pages.inbounds.ghostLinkActivation.mac'),
              children: data.fingerprint.mac_hashes.join(', ') || '—',
            },
          ]}
        />
      )}
      {data.configured && !waiting && (
        <>
          <Input.TextArea
            aria-label={t('pages.inbounds.ghostLinkActivation.note')}
            placeholder={t('pages.inbounds.ghostLinkActivation.note')}
            maxLength={320}
            value={note}
            onChange={(event) => setNote(event.target.value)}
          />
          <Button type="primary" loading={activate.isPending} onClick={() => activate.mutate()}>
            {t('pages.inbounds.ghostLinkActivation.apply')}
          </Button>
        </>
      )}
      {activate.error && <Alert type="error" showIcon title={activate.error.message} />}
    </Space>
  );
}
