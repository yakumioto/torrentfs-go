import { Button, PasswordInput, TextInput } from '@mantine/core';
import { IconKey, IconRadar } from '@tabler/icons-react';
import { FormEvent, useState } from 'react';
import { useAuth } from '../app/auth-context';

export function LoginPage() {
  const auth = useAuth();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setSubmitting(true);
    const success = await auth.login(username, password);
    setSubmitting(false);
    if (success) {
      setPassword('');
    }
  };

  return (
    <main className="auth-page">
      <section className="auth-card">
        <div className="auth-card__mark" aria-hidden="true"><IconRadar size={25} /></div>
        <p className="eyebrow">Protected control room</p>
        <h1>Sign in to your swarm.</h1>
        <p className="auth-card__copy">The interface shell is public. Torrent data and every management action stay behind the daemon's Bearer session.</p>
        <form onSubmit={submit}>
          <TextInput
            label="Username"
            value={username}
            onChange={(event) => setUsername(event.currentTarget.value)}
            autoComplete="username"
            required
            mb="md"
          />
          <PasswordInput
            label="Password"
            value={password}
            onChange={(event) => setPassword(event.currentTarget.value)}
            autoComplete="current-password"
            required
            mb="lg"
          />
          {auth.loginError !== '' && <div className="error-callout" role="alert" aria-live="polite"><IconKey size={15} aria-hidden="true" /> {auth.loginError}</div>}
          <Button type="submit" fullWidth color="mint" loading={submitting} mt="lg">Sign in</Button>
        </form>
        <p className="auth-card__footer">Tokens live in memory only. Refreshing this page starts a new connection check.</p>
      </section>
    </main>
  );
}
