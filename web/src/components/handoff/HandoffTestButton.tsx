import { Button, Group, Text } from '@mantine/core';
import { useState } from 'react';
import { useAuth } from '../../app/auth-context';
import { useTestHandoffTarget } from '../../queries/hooks';
import { userFacingHandoffError } from '../../utils/user-facing-error';

/**
 * The connection test runs against the configuration in force, not against the
 * unsaved draft, and says so: a user who just typed an address must save
 * before the test can pass.
 */
export function HandoffTestButton() {
  const auth = useAuth();
  const test = useTestHandoffTarget(auth.api);
  const [message, setMessage] = useState('');
  const [succeeded, setSucceeded] = useState(false);

  const run = () => {
    setMessage('');
    test.mutate({}, {
      onSuccess: (target) => {
        setSucceeded(true);
        setMessage(target.version !== undefined && target.version !== ''
          ? `连接成功，Transmission 版本 ${target.version}。`
          : '连接成功。');
      },
      onError: (error) => {
        setSucceeded(false);
        setMessage(userFacingHandoffError(error, '测试连接失败，请稍后重试。'));
      },
    });
  };

  return (
    <Group gap="xs" align="center" mt="xs">
      <Button size="xs" variant="light" color="torrent" onClick={run} loading={test.isPending}>测试连接</Button>
      <Text size="xs" c={succeeded ? 'teal' : 'red'}>{message}</Text>
      {message === '' && <Text size="xs" className="muted">测试使用当前生效的配置；改动需保存后才参与测试。</Text>}
    </Group>
  );
}
