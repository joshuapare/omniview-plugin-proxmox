import { useEffect, useMemo, useState } from 'react';
import {
  Banner,
  Button,
  Dialog,
  FormField,
  Grid,
  Input,
  Select,
  Spinner,
  Switch,
  Text,
  TextArea,
} from '@omniviewdev/base-ui';
import { jsonToValue, parseAppError, valueToJSON } from '@omniviewdev/runtime';
import { ResourceClient } from '@omniviewdev/runtime/api';
import { LuChevronDown, LuSettings2 } from 'react-icons/lu';

import { VM_CONFIG_CHANGED } from '../components/VMPendingConfig';
import { PLUGIN_ID } from '../constants';
import type { ActionTarget, MutateResource } from './actionDeps';
import {
  type ConfigField,
  type ConfigValues,
  changedValues,
  configFields,
  fieldErrors,
  initialValues,
  selectOptions,
  validate,
} from './guestConfig';

const dialogStyle = { maxWidth: 560 } as const;
const centerStyle = { display: 'flex', justifyContent: 'center', padding: '24px 0' } as const;
const bodyStyle = { display: 'flex', flexDirection: 'column', gap: 12 } as const;
const fullWidthStyle = { width: '100%' } as const;

type Loaded = { values: ConfigValues; digest: string };

type Props = {
  target: ActionTarget;
  mutate: MutateResource;
  onClose: () => void;
};

/**
 * Edits the common settings of a VM or container. Reads the guest's current
 * `/config` through the `config` action, then sends only the changed keys
 * (`''` deletes a key) with the config digest, so a concurrent edit is
 * rejected instead of overwritten.
 */
export default function EditConfigDialog({ target, mutate, onClose }: Props) {
  const fields = useMemo(() => configFields(target.resourceKey), [target.resourceKey]);
  const [loaded, setLoaded] = useState<Loaded>();
  const [loadError, setLoadError] = useState<string>();
  const [values, setValues] = useState<ConfigValues>({});
  const [serverErrors, setServerErrors] = useState<Record<string, string>>({});
  const [submitError, setSubmitError] = useState<string>();
  const [saving, setSaving] = useState(false);

  const { route, record } = target.record;
  useEffect(() => {
    let cancelled = false;
    // The client, not the runtime hook: a failure shows in the dialog, not as a notification too.
    ResourceClient.Action(PLUGIN_ID, {
      route,
      reference: record.reference,
      action: 'config',
      input: { kind: 'map', value: {} },
    })
      .then((result) => {
        if (cancelled) return;
        const { config, digest } = valueToJSON(result) as { config?: Record<string, unknown>; digest?: string };
        const initial = initialValues(fields, config ?? {});
        setLoaded({ values: initial, digest: digest ?? '' });
        setValues(initial);
      })
      .catch((error: unknown) => {
        if (!cancelled) setLoadError(parseAppError(error).detail);
      });
    return () => {
      cancelled = true;
    };
  }, [route, record.reference, fields]);

  const localErrors = useMemo(() => validate(fields, values), [fields, values]);
  const changes = useMemo(
    () => (loaded ? changedValues(fields, loaded.values, values) : {}),
    [fields, loaded, values],
  );
  const changed = Object.keys(changes).length > 0;
  const valid = Object.keys(localErrors).length === 0;

  const setValue = (key: string, value: string | boolean) => {
    setValues((prev) => ({ ...prev, [key]: value }));
    setServerErrors(({ [key]: _, ...rest }) => rest);
  };

  const submit = async () => {
    if (!loaded || !changed || !valid) return;
    setSaving(true);
    setSubmitError(undefined);
    try {
      const input: Record<string, unknown> = { ...changes };
      if (loaded.digest) input.digest = loaded.digest;
      await mutate({ route, reference: record.reference, operation: 'update', input: jsonToValue(input) });
      window.dispatchEvent(new CustomEvent(VM_CONFIG_CHANGED, {
        detail: { connection: route.connection, id: record.reference.address.id },
      }));
      onClose();
    } catch (error) {
      // The runtime already showed the error notification; keep the details next to the fields.
      const { byKey, rest } = fieldErrors(fields, parseAppError(error).detail);
      setServerErrors(byKey);
      setSubmitError(rest || undefined);
    } finally {
      setSaving(false);
    }
  };

  let body;
  if (loadError) {
    body = (
      <Banner color="danger" size="sm">
        <Banner.Content>Could not read the configuration: {loadError}</Banner.Content>
      </Banner>
    );
  } else if (!loaded) {
    body = (
      <div style={centerStyle}>
        <Spinner size="sm" />
      </div>
    );
  } else {
    body = (
      <div style={bodyStyle}>
        <Grid columns={12} spacing={3}>
          {fields.map((field) => (
            <Grid.Item key={field.key} span={field.kind === 'integer' || field.kind === 'switch' ? 6 : 12}>
              <ConfigInput
                field={field}
                value={values[field.key]}
                error={localErrors[field.key] ?? serverErrors[field.key]}
                disabled={saving}
                onChange={(value) => setValue(field.key, value)}
              />
            </Grid.Item>
          ))}
        </Grid>
        {submitError && (
          <Banner color="danger" size="sm">
            <Banner.Content>{submitError}</Banner.Content>
          </Banner>
        )}
      </div>
    );
  }

  return (
    <Dialog open onClose={onClose} size="md" style={dialogStyle}>
      <Dialog.Close aria-label="Close" />
      <Dialog.Title icon={<LuSettings2 size={16} />}>Edit {target.name}</Dialog.Title>
      <Dialog.Body>{body}</Dialog.Body>
      <Dialog.Footer>
        <Button size="sm" variant="outline" color="neutral" onClick={onClose}>
          Cancel
        </Button>
        <Button
          size="sm"
          variant="solid"
          color="brand"
          disabled={!changed || !valid || saving}
          onClick={() => void submit()}
        >
          {saving ? 'Saving…' : 'Save'}
        </Button>
      </Dialog.Footer>
    </Dialog>
  );
}

type InputProps = {
  field: ConfigField;
  value: string | boolean | undefined;
  error?: string;
  disabled: boolean;
  onChange: (value: string | boolean) => void;
};

function ConfigInput({ field, value, error, disabled, onChange }: InputProps) {
  const id = `proxmox-config-${field.key}`;

  if (field.kind === 'switch') {
    return (
      <div>
        <Switch id={id} size="sm" checked={value === true} disabled={disabled} onCheckedChange={onChange}>
          {field.label}
        </Switch>
        {field.description && (
          <Text size="xs" tone="muted">
            {field.description}
          </Text>
        )}
        {error && (
          <Text size="xs" tone="danger">
            {error}
          </Text>
        )}
      </div>
    );
  }

  const text = typeof value === 'string' ? value : '';
  let control;
  if (field.kind === 'textarea') {
    control = (
      <TextArea.Root size="sm" style={fullWidthStyle}>
        <TextArea.Control
          id={id}
          size="sm"
          rows={3}
          value={text}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        />
      </TextArea.Root>
    );
  } else if (field.kind === 'select') {
    const options = selectOptions(field, text);
    control = (
      <Select.Root
        size="sm"
        value={text}
        items={options}
        disabled={disabled}
        onValueChange={(next) => {
          if (next !== null && next !== undefined) onChange(String(next));
        }}
      >
        <Select.Trigger id={id} style={fullWidthStyle}>
          <Select.Value />
          <Select.Icon>
            <LuChevronDown aria-hidden />
          </Select.Icon>
        </Select.Trigger>
        <Select.Portal>
          <Select.Positioner sideOffset={4} style={{ zIndex: 'calc(var(--ov-z-modal, 9000) + 1)' }}>
            <Select.Popup>
              <Select.List>
                {options.map((opt) => (
                  <Select.Item key={opt.value} value={opt.value}>
                    <Select.ItemText>{opt.label}</Select.ItemText>
                  </Select.Item>
                ))}
              </Select.List>
            </Select.Popup>
          </Select.Positioner>
        </Select.Portal>
      </Select.Root>
    );
  } else {
    control = (
      <Input.Control
        id={id}
        size="sm"
        mono={field.kind === 'integer'}
        inputMode={field.kind === 'integer' ? 'numeric' : undefined}
        placeholder={field.placeholder}
        value={text}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  return (
    <FormField label={field.label} htmlFor={id} size="sm" description={field.description} error={error}>
      {control}
    </FormField>
  );
}
