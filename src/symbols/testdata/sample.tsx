interface Props { name: string }

export function Hello({ name }: Props) {
  return <div>{name}</div>
}

const Bye = () => <span>bye</span>
