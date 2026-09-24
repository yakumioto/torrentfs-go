export interface TorrentFileMergeResult {
  files: File[];
  accepted: File[];
  invalid: File[];
  duplicates: File[];
}

export function isTorrentFile(file: File): boolean {
  return file.name.toLowerCase().endsWith('.torrent');
}

export function torrentFileKey(file: File): string {
  return JSON.stringify([file.name, file.size, file.lastModified]);
}

export function mergeTorrentFiles(existing: File[], incoming: ArrayLike<File> | Iterable<File> | null | undefined): TorrentFileMergeResult {
  const files = [...existing];
  const accepted: File[] = [];
  const invalid: File[] = [];
  const duplicates: File[] = [];
  const keys = new Set(existing.map(torrentFileKey));

  for (const file of Array.from(incoming ?? [])) {
    if (!isTorrentFile(file)) {
      invalid.push(file);
      continue;
    }

    const key = torrentFileKey(file);
    if (keys.has(key)) {
      duplicates.push(file);
      continue;
    }

    keys.add(key);
    accepted.push(file);
    files.push(file);
  }

  return { files, accepted, invalid, duplicates };
}
