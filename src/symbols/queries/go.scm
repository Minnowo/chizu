; Patterns earlier in the file win when several match the same node.

(method_declaration
  receiver: (parameter_list
    (parameter_declaration
      type: [
        (type_identifier) @parent
        (pointer_type (type_identifier) @parent)
        (generic_type type: (type_identifier) @parent)
        (pointer_type (generic_type type: (type_identifier) @parent))
      ]))
  name: (field_identifier) @name) @definition.method

(method_declaration name: (field_identifier) @name) @definition.method

(function_declaration name: (identifier) @name) @definition.function

(type_spec name: (type_identifier) @name type: (struct_type)) @definition.struct
(type_spec name: (type_identifier) @name type: (interface_type)) @definition.interface
(type_spec name: (type_identifier) @name) @definition.type
(type_alias name: (type_identifier) @name) @definition.type

(source_file (const_declaration (const_spec name: (identifier) @name) @definition.constant))
(source_file (var_declaration (var_spec name: (identifier) @name) @definition.variable))
