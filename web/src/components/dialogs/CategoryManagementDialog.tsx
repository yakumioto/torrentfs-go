import { Button, Group, Modal, Stack, TextInput } from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import { useState } from 'react';
import { useAuth } from '../../app/auth-context';
import { useCategories, useCreateCategory } from '../../queries/hooks';
import { userFacingError } from '../../utils/user-facing-error';

export function CategoryManagementDialog({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const auth = useAuth();
  const categories = useCategories(auth.api, opened && auth.isReady);
  const create = useCreateCategory(auth.api);
  const [name, setName] = useState('');

  const close = () => {
    create.reset();
    setName('');
    onClose();
  };

  const submit = () => {
    if (name === '') {
      return;
    }
    create.mutate({ name }, { onSuccess: () => setName('') });
  };

  const listedCategories = [...(categories.data ?? [])];
  if (create.data !== undefined && !listedCategories.some((category) => category.name === create.data.name)) {
    listedCategories.push(create.data);
  }
  listedCategories.sort((a, b) => a.name.localeCompare(b.name));

  return (
    <Modal opened={opened} onClose={close} title="管理分类" centered closeButtonProps={{ 'aria-label': '关闭弹窗' }}>
      <Stack gap="md">
        <TextInput
          label="新建分类"
          placeholder="例如 movies"
          value={name}
          onChange={(event) => setName(event.currentTarget.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') {
              submit();
            }
          }}
          disabled={create.isPending}
        />
        {create.error !== null && create.error !== undefined && (
          <div className="error-callout" role="alert">
            <IconAlertTriangle size={15} aria-hidden="true" />
            {userFacingError(create.error, '创建分类失败，请检查名称后重试。')}
          </div>
        )}
        {categories.error !== null && categories.error !== undefined && (
          <div className="error-callout" role="alert">
            <IconAlertTriangle size={15} aria-hidden="true" />
            {userFacingError(categories.error, '分类列表暂不可用。')}
          </div>
        )}
        {create.data !== undefined && <div role="status">已创建分类“{create.data.name}”。</div>}
        <div aria-label="已有分类">
          <strong>已有分类</strong>
          {categories.isPending && <p className="muted">正在加载分类…</p>}
          {!categories.isPending && listedCategories.length === 0 && <p className="muted">还没有分类。</p>}
          {listedCategories.length > 0 && (
            <ul>
              {listedCategories.map((category) => <li key={category.name}>{category.name}</li>)}
            </ul>
          )}
        </div>
        <Group justify="flex-end">
          <Button variant="subtle" color="gray" onClick={close}>关闭</Button>
          <Button color="torrent" onClick={submit} loading={create.isPending} disabled={name === ''}>创建分类</Button>
        </Group>
      </Stack>
    </Modal>
  );
}
