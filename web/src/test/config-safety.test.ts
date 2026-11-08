import { describe, expect, it } from 'vitest';
import {
  dangerousCategoryLabel,
  dangerousConfigChanges,
  dangerousConfigKey,
} from '../utils/config-safety';

describe('dangerous configuration keys', () => {
  // The list comes from the save-safety ticket, not from the implementation.
  const ticketPaths = [
    'http.listen_addr',
    'http.auth.enabled',
    'backend',
    'smb.listen_addr',
    'smb.strm',
    'fuse.mountpoint',
    'fuse.strm_mountpoint',
    'connections.listen_port',
  ];

  it('flags exactly the keys named by the ticket with a user-facing impact', () => {
    expect(dangerousConfigChanges(ticketPaths).map((change) => change.path)).toEqual(ticketPaths);
    for (const path of ticketPaths) {
      const change = dangerousConfigKey(path);
      expect(change).toBeDefined();
      if (change === undefined) {
        continue;
      }
      expect(change.impact).not.toBe('');
      expect(dangerousCategoryLabel(change.category)).not.toBe('');
    }
    expect(dangerousConfigChanges(['log.level', 'cache.capacity', 'password'])).toEqual([]);
    expect(dangerousConfigKey('log.level')).toBeUndefined();
  });

  it('keeps the order of the changes so the confirmation follows the page', () => {
    expect(dangerousConfigChanges(['fuse.mountpoint', 'log.level', 'backend']).map((change) => change.path))
      .toEqual(['fuse.mountpoint', 'backend']);
  });
});
