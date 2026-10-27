import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { fetchEntryURL, HostEntry, resolveHostEntryURL } from './HostEntry';

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

const hostURL = 'https://h1234567.hosts.test';

it('obtains a shared terminal ticket only for a currently permitted host', async () => {
  vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{ id: 'h1234567', name: '共享电脑', state: 'running', url: hostURL, permissions: ['terminal:control'] }] });
  const ticket = vi.spyOn(portalApi, 'issueObservationTicket').mockResolvedValue({ url: `${hostURL}/__ch/auth?t=one-time&next=%2F__ch%2Fterminal%2F`, expires_at: '2027-10-25T00:01:00Z' });
  expect(await fetchEntryURL('h1234567', 'terminal')).toBe(`${hostURL}/__ch/auth?t=one-time&next=%2F__ch%2Fterminal%2F`);
  expect(ticket).toHaveBeenCalledWith('h1234567', 'terminal');
});

it('rejects a revoked shared permission before issuing a ticket', async () => {
  vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{ id: 'h1234567', name: '共享电脑', state: 'running', url: hostURL, permissions: ['terminal:control'] }] });
  const ticket = vi.spyOn(portalApi, 'issueObservationTicket');
  await expect(fetchEntryURL('h1234567', 'desktop')).rejects.toThrow('这台共享电脑已不可访问');
  expect(ticket).not.toHaveBeenCalled();
});

it('uses the owner entry endpoint for an owned host', async () => {
  vi.spyOn(portalApi, 'host').mockResolvedValue({ id: 'h1234567', url: hostURL } as Awaited<ReturnType<typeof portalApi.host>>);
  const enter = vi.spyOn(portalApi, 'enterHost').mockResolvedValue({ url: `${hostURL}/__ch/auth?t=owner-ticket`, expires_at: '2027-10-25T00:01:00Z' });
  expect(await fetchEntryURL('h1234567', null)).toBe(`${hostURL}/__ch/auth?t=owner-ticket`);
  expect(enter).toHaveBeenCalledWith('h1234567');
});

it.each([
  'https://foreign.test/__ch/auth?t=x',
  `${hostURL}/__ch/auth?t=x&t=y`,
  `${hostURL}/__ch/auth`,
  `${hostURL}/__ch/auth?t=x#fragment`,
  `${hostURL}/other?t=x`,
  `${hostURL}/__ch/auth?t=x&redirect=https%3A%2F%2Fforeign.test`,
  `${hostURL}/__ch/auth?t=x&next=%2F%2Fforeign.test`,
  `${hostURL}/__ch/auth?t=x&next=%2F%5Cforeign.test`,
  `${hostURL}/__ch/auth?t=x&next=%2Fone&next=%2Ftwo`,
])('rejects a malformed or foreign Host ticket URL: %s', (ticketURL) => {
  expect(() => resolveHostEntryURL(ticketURL, hostURL)).toThrow('服务器返回了无效的云端电脑入口');
});

it('requires an HTTPS Host origin without a path or query', () => {
  expect(() => resolveHostEntryURL('http://host.test/__ch/auth?t=x', 'http://host.test')).toThrow('服务器返回了无效的云端电脑入口');
  expect(() => resolveHostEntryURL(`${hostURL}/__ch/auth?t=x`, `${hostURL}/extra`)).toThrow('服务器返回了无效的云端电脑入口');
});

it('allows the local development Host only from the local Portal', () => {
  const local = 'http://h1234567.hosts.localhost';
  expect(resolveHostEntryURL(`${local}/__ch/auth?t=x`, local, 'portal.localhost')).toBe(`${local}/__ch/auth?t=x`);
  expect(resolveHostEntryURL(`${local}/__ch/auth?t=x`, local, 'localhost')).toBe(`${local}/__ch/auth?t=x`);
  expect(() => resolveHostEntryURL(`${local}/__ch/auth?t=x`, local, 'portal.example.com')).toThrow('服务器返回了无效的云端电脑入口');
  expect(() => resolveHostEntryURL('http://host.test/__ch/auth?t=x', 'http://host.test', 'portal.localhost')).toThrow('服务器返回了无效的云端电脑入口');
});

it('shows an error for an unsupported handoff target', async () => {
  const shared = vi.spyOn(portalApi, 'sharedHosts');
  render(<MemoryRouter initialEntries={['/host-entry/h1234567?target=admin']}><Routes><Route path="/host-entry/:id" element={<HostEntry />} /></Routes></MemoryRouter>);
  expect(await screen.findByRole('alert')).toHaveTextContent('云端电脑入口参数无效');
  expect(shared).not.toHaveBeenCalled();
});
