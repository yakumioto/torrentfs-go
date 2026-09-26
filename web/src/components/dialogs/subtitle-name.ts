/**
 * Client-side mirror of the server's subtitle naming rules. It exists only to
 * warn before an upload; the server recomputes the target and remains the sole
 * authority, so a disagreement here can never widen what is accepted.
 */
const SUBTITLE_EXTENSIONS = ['.srt', '.ass', '.vtt'] as const;

function extensionOf(name: string): string {
  const index = name.lastIndexOf('.');
  return index <= 0 ? '' : name.slice(index);
}

function basenameOf(path: string): string {
  const index = path.lastIndexOf('/');
  return index === -1 ? path : path.slice(index + 1);
}

function directoryOf(path: string): string {
  const index = path.lastIndexOf('/');
  return index === -1 ? '' : path.slice(0, index);
}

export function subtitleExtension(name: string): string | undefined {
  const extension = extensionOf(name);
  return (SUBTITLE_EXTENSIONS as readonly string[]).includes(extension) ? extension : undefined;
}

/** The basename a subtitle must carry to belong to videoPath. */
export function expectedSubtitleStem(videoPath: string): string {
  const base = basenameOf(videoPath);
  const extension = extensionOf(base);
  return extension === '' ? base : base.slice(0, -extension.length);
}

export function subtitleBasenameMismatch(videoPath: string, fileName: string): boolean {
  const extension = subtitleExtension(fileName);
  if (extension === undefined) {
    return true;
  }
  const stem = fileName.slice(0, -extension.length);
  return stem !== expectedSubtitleStem(videoPath);
}

/** The torrent-relative path a subtitle will occupy for videoPath. */
export function subtitleTargetPath(videoPath: string, fileName: string): string {
  const directory = directoryOf(videoPath);
  return directory === '' ? fileName : `${directory}/${fileName}`;
}
