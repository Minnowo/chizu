LIMIT = 10
lowercase = 1


class Greeter:
    def hello(self):
        return 1

    @staticmethod
    def make():
        return Greeter()

    class Inner:
        def deep(self):
            pass


def main():
    def nested():
        pass
