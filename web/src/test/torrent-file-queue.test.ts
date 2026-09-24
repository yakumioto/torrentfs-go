import { describe, expect, it } from 'vitest';
import { isTorrentFile, mergeTorrentFiles, torrentFileKey } from '../components/dialogs/torrent-file-queue';

function file(name: string, size = 1, lastModified = 1): File {
  return new File(['x'.repeat(size)], name, { lastModified });
}

describe('torrent file queue', () => {
  it('accepts torrent extensions case-insensitively without relying on MIME type', () => {
    expect(isTorrentFile(file('A.TORRENT'))).toBe(true);
    expect(isTorrentFile(new File(['payload'], 'metadata', { type: 'application/x-bittorrent' }))).toBe(false);
  });

  it('keeps valid files and reports invalid files and metadata duplicates', () => {
    const existing = file('existing.torrent');
    const same = file('existing.torrent');
    const valid = file('new.torrent');
    const invalid = file('notes.txt');

    const result = mergeTorrentFiles([existing], [same, valid, invalid]);

    expect(result.files).toEqual([existing, valid]);
    expect(result.accepted).toEqual([valid]);
    expect(result.duplicates).toEqual([same]);
    expect(result.invalid).toEqual([invalid]);
  });

  it('does not treat files with different metadata as duplicates', () => {
    const first = file('same.torrent', 1, 1);
    const second = file('same.torrent', 2, 1);
    const third = file('same.torrent', 1, 2);

    expect(torrentFileKey(first)).not.toBe(torrentFileKey(second));
    expect(torrentFileKey(first)).not.toBe(torrentFileKey(third));
    expect(mergeTorrentFiles([first], [second, third]).accepted).toEqual([second, third]);
  });

  it('leaves an existing queue unchanged for empty or cancelled input', () => {
    const existing = file('existing.torrent');

    expect(mergeTorrentFiles([existing], []).files).toEqual([existing]);
    expect(mergeTorrentFiles([existing], null).files).toEqual([existing]);
    expect(mergeTorrentFiles([existing], undefined).files).toEqual([existing]);
  });
});
