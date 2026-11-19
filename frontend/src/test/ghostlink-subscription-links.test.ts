import { describe, expect, it } from 'vitest';

import { classifyGhostLinkService } from '@/lib/inbounds/ghostlink-status';
import { buildInboundSubscriptionLinks } from '@/lib/inbounds/subscription-links';

const services = {
  enable: false,
  subURI: 'https://sub.example/',
  subJsonEnable: true,
  subJsonURI: 'https://json.example/',
  subClashEnable: true,
  subClashURI: 'https://mihomo.example/',
};

describe('GhostLink inbound subscription gating', () => {
  it('shows only the Mihomo URL when standard subscription is disabled', () => {
    expect(buildInboundSubscriptionLinks('ghostlink', 'client-sub', services)).toEqual({
      standard: '',
      json: '',
      mihomo: 'https://mihomo.example/client-sub',
    });
  });

  it('does not expose links when the applicable subscription is disabled', () => {
    expect(
      buildInboundSubscriptionLinks('ghostlink', 'client-sub', {
        ...services,
        subClashEnable: false,
      }),
    ).toEqual({ standard: '', json: '', mihomo: '' });
    expect(
      buildInboundSubscriptionLinks('vless', 'client-sub', {
        ...services,
        enable: false,
      }),
    ).toEqual({ standard: '', json: 'https://json.example/client-sub', mihomo: '' });
  });
});

describe('GhostLink service state', () => {
  const healthy = {
    running: true,
    ready: true,
    runtimeReady: true,
    desiredRevision: 'config-a',
    activeRevision: 'config-a',
    desiredUserRevision: 'users-a',
    runtimeRevision: 'users-a',
  };

  it('distinguishes healthy, degraded, stopped, and error states', () => {
    expect(classifyGhostLinkService(healthy)).toBe('healthy');
    expect(classifyGhostLinkService({ ...healthy, activeRevision: 'old' })).toBe('degraded');
    expect(classifyGhostLinkService({ ...healthy, runtimeRevision: '' })).toBe('degraded');
    expect(classifyGhostLinkService({ ...healthy, pendingOperationId: 'op-1' })).toBe('degraded');
    expect(classifyGhostLinkService({ ...healthy, ready: false })).toBe('stopped');
    expect(classifyGhostLinkService({ ...healthy, policyErrorCode: 'usage_pending' })).toBe(
      'error',
    );
  });
});
