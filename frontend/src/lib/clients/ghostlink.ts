import { RandomUtil } from '@/utils';

export const GHOSTLINK_SECRET_MIN_BYTES = 32;
export const GHOSTLINK_SECRET_MAX_BYTES = 1024;
export const GHOSTLINK_UUID_PROTOCOLS = new Set(['vmess', 'vless', 'tuic']);

export function ghostLinkSecretByteLength(value: string): number {
  return new TextEncoder().encode(value).length;
}

export function generateGhostLinkSecret(): string {
  return RandomUtil.randomSeq(48, { type: 'hex' });
}

export function isValidGhostLinkClientId(value: string): boolean {
  if (value.length < 1 || value.length > 128 || value.trim() !== value) return false;
  return Array.from(value).every((char) => {
    const code = char.codePointAt(0) ?? 0;
    return code >= 0x21 && code <= 0x7e && char !== ':';
  });
}

export function isValidGhostLinkSecret(value: string): boolean {
  const size = ghostLinkSecretByteLength(value);
  return size >= GHOSTLINK_SECRET_MIN_BYTES && size <= GHOSTLINK_SECRET_MAX_BYTES;
}

export function hasGhostLinkMtprotoConflict(protocols: Iterable<string>): boolean {
  let hasGhostLink = false;
  let hasMtproto = false;
  for (const protocol of protocols) {
    if (protocol === 'ghostlink') hasGhostLink = true;
    if (protocol === 'mtproto') hasMtproto = true;
  }
  return hasGhostLink && hasMtproto;
}

export function isStandardXrayUUID(value: string): boolean {
  if (value.length !== 32 && value.length !== 36) return false;
  const hex = value.replaceAll('-', '');
  if (hex.length !== 32 || !/^[0-9a-fA-F]{32}$/.test(hex)) return false;
  return (
    value.length === 32 || /^[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$/.test(value)
  );
}

export function hasGhostLinkUUIDProtocolMix(protocols: Iterable<string>): boolean {
  let hasGhostLink = false;
  let hasUUIDProtocol = false;
  for (const protocol of protocols) {
    if (protocol === 'ghostlink') hasGhostLink = true;
    if (GHOSTLINK_UUID_PROTOCOLS.has(protocol)) hasUUIDProtocol = true;
  }
  return hasGhostLink && hasUUIDProtocol;
}

export function withPreservedGhostLinkSecret(
  payload: Record<string, unknown>,
  secret: string,
  isEdit: boolean,
): Record<string, unknown> {
  if (secret !== '' || !isEdit) {
    payload.secret = secret;
  }
  return payload;
}
