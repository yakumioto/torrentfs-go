import { durationInputError, parseDuration } from './duration';
import { parseByteQuantity, parseUploadRate, type QuantityResult } from './quantities';

function quantityError(result: QuantityResult): string | undefined {
  if ('value' in result) {
    return undefined;
  }
  switch (result.error) {
    case 'empty':
      return undefined;
    case 'missing_unit':
      return '请输入带单位的字节量，例如 32MB 或 2GiB。';
    case 'unknown_unit':
      return '仅支持 B、KB、MB、GB、TB、KiB、MiB、GiB 或 TiB。';
    case 'overflow':
      return '字节量超出浏览器可精确编辑的范围。';
    case 'missing_rate_suffix':
      return '请输入带单位的速率，例如 1MiB/s。';
    case 'invalid_format':
      return '请输入整数，不支持小数或科学计数法。';
  }
}

function rateError(result: QuantityResult): string | undefined {
  if ('value' in result) {
    return undefined;
  }
  if (result.error === 'missing_rate_suffix') {
    return '请输入带单位的速率，例如 1MiB/s。';
  }
  return quantityError(result);
}

/**
 * Client-side shape check for the configuration kinds that have a unit or a
 * duration syntax.
 *
 * The backend stays the authority: this only catches a typo before the save
 * request, with the same syntax the configuration file accepts. An empty
 * value is never rejected here because whether it is allowed is a rule of the
 * configuration key, not of its type.
 */
export function configValueInputError(kind: string, raw: string): string | undefined {
  const value = raw.trim();
  if (value === '') {
    return undefined;
  }
  switch (kind) {
    case 'byte_quantity':
      return quantityError(parseByteQuantity(value));
    case 'byte_rate':
      return rateError(parseUploadRate(value));
    case 'duration': {
      const message = durationInputError(parseDuration(value));
      return message === '' ? undefined : message;
    }
    default:
      return undefined;
  }
}
