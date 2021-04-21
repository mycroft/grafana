import { UseFormReturn, FieldValues, FieldErrors } from 'react-hook-form';
export { SubmitHandler as FormsOnSubmit, FieldErrors as FormFieldErrors } from 'react-hook-form';

export type FormAPI<T> = Pick<UseFormReturn<T>, 'register' | 'control' | 'formState' | 'getValues' | 'watch'> & {
  errors: FieldErrors<T>;
};

type FieldArrayValue = Partial<FieldValues> | Array<Partial<FieldValues>>;

export interface FieldArrayApi {
  fields: Array<Record<string, any>>;
  append: (value: FieldArrayValue) => void;
  prepend: (value: FieldArrayValue) => void;
  remove: (index?: number | number[]) => void;
  swap: (indexA: number, indexB: number) => void;
  move: (from: number, to: number) => void;
  insert: (index: number, value: FieldArrayValue) => void;
}
