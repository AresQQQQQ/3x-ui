import { describe, expect, it } from 'vitest';

import {
  generateGhostLinkSecret,
  ghostLinkSecretByteLength,
  hasGhostLinkMtprotoConflict,
  hasGhostLinkUUIDProtocolMix,
  isValidGhostLinkClientId,
  isValidGhostLinkSecret,
  isStandardXrayUUID,
  withPreservedGhostLinkSecret,
} from '@/lib/clients/ghostlink';

describe('GhostLink client validation', () => {
  it('accepts printable Client IDs and rejects reserved/whitespace forms', () => {
    expect(isValidGhostLinkClientId('client-01')).toBe(true);
    for (const invalid of ['', 'has space', 'has:colon', 'with\nnewline', ' leading']) {
      expect(isValidGhostLinkClientId(invalid)).toBe(false);
    }
  });

  it('enforces Secret UTF-8 byte limits and generates a strong-sized value', () => {
    const generated = generateGhostLinkSecret();
    expect(ghostLinkSecretByteLength(generated)).toBe(48);
    expect(isValidGhostLinkSecret(generated)).toBe(true);
    expect(isValidGhostLinkSecret('short')).toBe(false);
    expect(isValidGhostLinkSecret('é'.repeat(16))).toBe(true);
  });

  it('detects MTProto conflict and keeps an omitted edit Secret unchanged', () => {
    expect(hasGhostLinkMtprotoConflict(['vless', 'ghostlink', 'mtproto'])).toBe(true);
    expect(hasGhostLinkMtprotoConflict(['vless', 'ghostlink'])).toBe(false);

    const editPayload: Record<string, unknown> = {};
    withPreservedGhostLinkSecret(editPayload, '', true);
    expect(editPayload).not.toHaveProperty('secret');

    const createPayload: Record<string, unknown> = {};
    withPreservedGhostLinkSecret(createPayload, 'provided-secret', false);
    expect(createPayload.secret).toBe('provided-secret');
  });

  it('requires a standard UUID only when GhostLink shares a UUID identity protocol', () => {
    expect(isStandardXrayUUID('11111111-2222-4333-8444-555555555555')).toBe(true);
    expect(isStandardXrayUUID('11111111222243338444555555555555')).toBe(true);
    for (const invalid of [
      'custom-client-01',
      '11111111-2222-4333-8444-55555555555',
      `11111111222243338444555555555555abcd`,
      '{11111111-2222-4333-8444-555555555555}',
      '11111111_2222_4333_8444_555555555555',
    ]) {
      expect(isStandardXrayUUID(invalid)).toBe(false);
    }

    expect(hasGhostLinkUUIDProtocolMix(['ghostlink', 'vless'])).toBe(true);
    expect(hasGhostLinkUUIDProtocolMix(['ghostlink', 'vmess'])).toBe(true);
    expect(hasGhostLinkUUIDProtocolMix(['ghostlink', 'tuic'])).toBe(true);
    expect(hasGhostLinkUUIDProtocolMix(['ghostlink', 'trojan', 'hysteria'])).toBe(false);
    expect(hasGhostLinkUUIDProtocolMix(['vless'])).toBe(false);
  });
});
