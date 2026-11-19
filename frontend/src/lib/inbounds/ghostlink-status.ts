export interface GhostLinkStatusData {
  unit?: string;
  expected?: boolean;
  running?: boolean;
  ready?: boolean;
  runtimeReady?: boolean;
  listen?: string;
  desiredRevision?: string;
  activeRevision?: string;
  reason?: string;
  mainPid?: number;
  runtimePid?: number;
  instanceId?: string;
  desiredUserRevision?: string;
  runtimeRevision?: string;
  pendingOperationId?: string;
  pendingPhase?: string;
  usageState?: string;
  usageErrorCode?: string;
  usageLastSnapshotAt?: number;
  policyState?: string;
  policyErrorCode?: string;
}

export type GhostLinkServiceState = 'healthy' | 'degraded' | 'stopped' | 'error';

export function classifyGhostLinkService(data: GhostLinkStatusData): GhostLinkServiceState {
  const serviceUp = !!data.running && !!data.ready && !!data.runtimeReady;
  const revisionMatches =
    !!data.desiredRevision && !!data.activeRevision && data.desiredRevision === data.activeRevision;
  const userRevisionMatches =
    !!data.desiredUserRevision &&
    !!data.runtimeRevision &&
    data.desiredUserRevision === data.runtimeRevision;
  const explicitError = !!(
    data.usageErrorCode ||
    data.policyErrorCode ||
    data.usageState === 'pending' ||
    data.policyState === 'pending_usage'
  );
  if (explicitError) return 'error';
  if (!serviceUp) return 'stopped';
  if (!revisionMatches || !userRevisionMatches || data.pendingOperationId) return 'degraded';
  return 'healthy';
}
