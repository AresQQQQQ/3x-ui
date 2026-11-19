import { describe, it, expect, vi } from 'vitest';
import { fireEvent, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import ClientFormModal from '@/pages/clients/ClientFormModal';
import { renderWithProviders } from './test-utils';
import type { ClientRecord, InboundOption } from '@/hooks/useClients';

// ClientFormModal reads server state via react-query (useFail2banStatusQuery),
// so it needs a QueryClientProvider on top of the shared ThemeProvider wrapper.
function renderModal({
  mode = 'add',
  client = null,
  inbounds = [],
  attachedIds = [],
}: {
  mode?: 'add' | 'edit';
  client?: ClientRecord | null;
  inbounds?: InboundOption[];
  attachedIds?: number[];
} = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithProviders(
    <QueryClientProvider client={queryClient}>
      <ClientFormModal
        open
        mode={mode}
        client={client}
        inbounds={inbounds}
        attachedIds={attachedIds}
        save={vi.fn().mockResolvedValue(null)}
        onOpenChange={() => {}}
      />
    </QueryClientProvider>,
  );
}

function openCredentialsTab() {
  const tab = Array.from(document.querySelectorAll('.ant-tabs-tab')).find(
    (t) => (t.textContent ?? '').trim() === 'Credentials',
  );
  if (!tab) throw new Error('Credentials tab not found');
  fireEvent.click(tab);
}

function tooltipIconForLabel(label: string): HTMLElement {
  const labelEl = Array.from(document.querySelectorAll('.ant-form-item-label label')).find(
    (l) => (l.textContent ?? '').trim() === label,
  );
  const item = labelEl?.closest('.ant-form-item') as HTMLElement | null;
  if (!item) throw new Error(`Form item not found for label: ${label}`);
  const tip = item.querySelector('.ant-form-item-tooltip') as HTMLElement | null;
  if (!tip) throw new Error(`No tooltip on form item: ${label}`);
  return tip;
}

describe('ClientFormModal credential tooltips', () => {
  it('explains which protocols consume the Password field', async () => {
    renderModal();
    openCredentialsTab();

    const tip = tooltipIconForLabel('Password');
    fireEvent.mouseEnter(tip);

    await waitFor(() => {
      expect(document.body.textContent).toContain(
        'Used by Trojan, Shadowsocks, TUIC, and MASQUE clients; ignored for VLESS, VMess, Hysteria, and WireGuard.',
      );
    });
  });

  it('explains that Hysteria Auth is the credential Hysteria actually uses', async () => {
    renderModal();
    openCredentialsTab();

    const tip = tooltipIconForLabel('Hysteria Auth');
    fireEvent.mouseEnter(tip);

    await waitFor(() => {
      expect(document.body.textContent).toContain(
        'Credential used only by Hysteria clients. Trojan and Shadowsocks use the Password field instead.',
      );
    });
  });

  it('hides package auto-renew for GhostLink mixed with a password protocol', async () => {
    renderModal({
      mode: 'edit',
      client: {
        email: 'ghost-mixed@test',
        subId: 'ghost-mixed-sub',
        uuid: 'custom-client-01',
        secret: '0123456789abcdef0123456789abcdef',
        totalGB: 0,
        expiryTime: 0,
        enable: true,
        inboundIds: [1, 2],
      } as ClientRecord,
      inbounds: [
        { id: 1, protocol: 'ghostlink', enable: true, tag: 'gl', remark: 'GhostLink' },
        { id: 2, protocol: 'shadowsocks', enable: true, tag: 'ss', remark: 'Shadowsocks' },
      ] as InboundOption[],
      attachedIds: [1, 2],
    });

    await waitFor(() => {
      expect(document.body.textContent).toContain('Traffic Reset');
    });
    expect(document.body.textContent).not.toContain('Auto renewal');
  });
});
