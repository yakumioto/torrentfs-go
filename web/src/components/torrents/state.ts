import { IconAlertTriangle, IconCircleCheck, IconDownload, IconLoader, IconQuestionMark } from '@tabler/icons-react';

interface StateMeta {
  label: string;
  icon: typeof IconLoader;
}

export const stateMeta: Record<string, StateMeta> = {
  adding: { label: 'Adding', icon: IconLoader },
  downloading: { label: 'Downloading', icon: IconDownload },
  seeding: { label: 'Seeding', icon: IconCircleCheck },
  error: { label: 'Error', icon: IconAlertTriangle },
  deleting: { label: 'Deleting', icon: IconLoader },
  delete_failed: { label: 'Delete failed', icon: IconAlertTriangle },
  unknown: { label: 'Unknown', icon: IconQuestionMark },
};
