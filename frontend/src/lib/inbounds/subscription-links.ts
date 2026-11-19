export interface InboundSubscriptionSettings {
  enable?: boolean;
  subURI?: string;
  subJsonURI?: string;
  subJsonEnable?: boolean;
  subClashURI?: string;
  subClashEnable?: boolean;
}

export interface InboundSubscriptionLinks {
  standard: string;
  json: string;
  mihomo: string;
}

export function buildInboundSubscriptionLinks(
  protocol: string,
  subId: string | undefined,
  settings: InboundSubscriptionSettings | undefined,
): InboundSubscriptionLinks {
  if (!subId || !settings) {
    return { standard: '', json: '', mihomo: '' };
  }
  if (protocol === 'ghostlink') {
    return {
      standard: '',
      json: '',
      mihomo: settings.subClashEnable && settings.subClashURI ? settings.subClashURI + subId : '',
    };
  }
  return {
    standard: settings.enable && settings.subURI ? settings.subURI + subId : '',
    json: settings.subJsonEnable && settings.subJsonURI ? settings.subJsonURI + subId : '',
    mihomo: '',
  };
}
